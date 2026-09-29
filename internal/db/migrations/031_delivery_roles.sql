-- 031_delivery_roles.sql - least-privilege delivery roles and the
-- cross-driver claim functions.
-- DONE: Vercel/Supabase split / STANDARDS A, D, E, F
--
-- WHY A DEDICATED ROLE: agent_reminders is created with FORCE ROW
-- LEVEL SECURITY (012_agent_reminders.sql:22) and its policy is
-- `tenant_id = current_setting('app.tenant_id', true)`. FORCE applies
-- to the table owner, so a SECURITY DEFINER function owned by postgres
-- still cannot read across tenants - a cross-tenant sweeper has no
-- single app.tenant_id to set. The Go scheduler gets around this with
-- adminPool, a SUPERUSER DSN (main.go).
--
-- The Edge Function gets a strictly narrower path than that:
--
--   delivery_worker  NOLOGIN BYPASSRLS. Owns the functions below, so
--                    they execute with its privileges and bypass
--                    FORCE RLS. Cannot log in.
--   delivery_caller  LOGIN NOBYPASSRLS. USAGE on private, EXECUTE on
--                    the functions below, nothing else. No table
--                    grants, no schema public access, no superuser.
--
-- The claim functions return bot tokens as BASE64 CIPHERTEXT. The vault
-- key never enters Postgres - the Edge Function decrypts it. That keeps
-- service_role off tenant tables (ARCHITECTURE.md 7, rule 2) and means
-- a database dump is not enough to impersonate a tenant's bot.
--
-- Password for delivery_caller is set out of band (ops/set-delivery-password.sql)
-- and lives in Supabase Vault. Never here.
--
-- Idempotent.

CREATE SCHEMA IF NOT EXISTS private;
REVOKE ALL ON SCHEMA private FROM PUBLIC;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'delivery_worker') THEN
    CREATE ROLE delivery_worker NOLOGIN BYPASSRLS;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'delivery_caller') THEN
    CREATE ROLE delivery_caller LOGIN NOBYPASSRLS;
  END IF;
END $$;

GRANT USAGE ON SCHEMA private TO delivery_caller;
GRANT CONNECT ON DATABASE postgres TO delivery_caller;

-- ---------------------------------------------------------------------------
-- Claim due reminders.
--
-- Atomically flips status 'pending' -> 'sending' and stamps claimed_at.
-- Drivers only ever read 'pending', so a claimed row is invisible to
-- the other driver. Returns ciphertext, never a plaintext token.
-- Rows with no owning bot are returned with token_enc = null so the
-- caller can dead-letter them (matches deliver()'s errHandledDead).
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION private.claim_due_reminders(p_limit integer DEFAULT 20)
RETURNS SETOF jsonb
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
DECLARE
  v_ids  uuid[];
  v_ts   timestamptz := pg_catalog.clock_timestamp();
BEGIN
  SELECT coalesce(pg_catalog.array_agg(c.id), '{}'::uuid[])
    INTO v_ids
  FROM (
    SELECT r.id
    FROM public.agent_reminders r
    WHERE r.status = 'pending'
      AND r.remind_at <= pg_catalog.now()
    ORDER BY r.remind_at
    LIMIT GREATEST(p_limit, 0)
    FOR UPDATE SKIP LOCKED
  ) c;

  IF v_ids = '{}'::uuid[] THEN
    RETURN;
  END IF;

  RETURN QUERY
  WITH claimed AS (
    UPDATE public.agent_reminders r
       SET status = 'sending',
           claimed_at = v_ts
     WHERE r.id = ANY(v_ids)
    RETURNING r.id, r.tenant_id, r.chat_id, r.text, r.attempts, r.bot_id
  )
  SELECT jsonb_build_object(
    'id',         c.id,
    'tenant_id',  c.tenant_id,
    'chat_id',    c.chat_id,
    'text',       c.text,
    'token_enc',  CASE WHEN tb.token_enc IS NULL THEN NULL
                        ELSE pg_catalog.encode(tb.token_enc, 'base64') END,
    'attempts',   c.attempts,
    'claimed_at', v_ts
  )
  FROM claimed c
  LEFT JOIN public.telegram_bots tb ON tb.id = c.bot_id;
END;
$$;

-- ---------------------------------------------------------------------------
-- Terminal outcomes. Scoped by the claim token (claimed_at) so a send
-- that outlived the reaper window cannot mark a row the reaper already
-- handed back. Returns true when the row was still ours.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION private.complete_reminder(
  p_id          uuid,
  p_claimed_at  timestamptz,
  p_status      text
)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
DECLARE
  v_n integer;
BEGIN
  IF p_status NOT IN ('sent', 'dead', 'cancelled') THEN
    RETURN false;
  END IF;
  UPDATE public.agent_reminders r
     SET status = p_status,
         claimed_at = NULL
   WHERE r.id = p_id
     AND r.status = 'sending'
     AND r.claimed_at = p_claimed_at;
  GET DIAGNOSTICS v_n = ROW_COUNT;
  RETURN v_n > 0;
END;
$$;

-- ---------------------------------------------------------------------------
-- Transient failure. Mirrors Scheduler.bumpAttempts (reminder.go:208)
-- including maxAttempts = 5, with one change forced by the claim design:
-- the old `ELSE status` left the row pending implicitly, so a claimed
-- row must be reset explicitly or it never becomes visible again.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION private.fail_reminder(
  p_id          uuid,
  p_claimed_at  timestamptz,
  p_max         integer DEFAULT 5
)
RETURNS text
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
DECLARE
  v_status text;
BEGIN
  UPDATE public.agent_reminders r
     SET attempts = r.attempts + 1,
         status = CASE WHEN r.attempts + 1 >= p_max THEN 'dead' ELSE 'pending' END,
         claimed_at = NULL
   WHERE r.id = p_id
     AND r.status = 'sending'
     AND r.claimed_at = p_claimed_at
  RETURNING r.status INTO v_status;
  RETURN v_status;
END;
$$;

-- ---------------------------------------------------------------------------
-- Reaper: hand back claims orphaned by a crashed driver. Only rows whose
-- claim is older than the window are touched, so a slow-but-alive driver
-- is never robbed mid-send. 10 minutes is ~75x the per-row send budget
-- in deliver() (8s) and well past a Telegram 429 retry.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION private.reap_stale_claims(
  p_older_than interval DEFAULT interval '10 minutes'
)
RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
DECLARE
  v_n integer;
BEGIN
  UPDATE public.agent_reminders r
     SET status = 'pending',
         claimed_at = NULL
   WHERE r.status = 'sending'
     AND r.claimed_at < pg_catalog.now() - p_older_than;
  GET DIAGNOSTICS v_n = ROW_COUNT;
  RETURN v_n;
END;
$$;

-- ---------------------------------------------------------------------------
-- Cheap existence probe. The pg_cron job calls this first so it only
-- invokes the Edge Function when something is actually due. Same call
-- from the Go ticker's own idle path.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION private.any_reminders_due()
RETURNS boolean
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = ''
AS $$
  SELECT EXISTS (
    SELECT 1 FROM public.agent_reminders r
    WHERE r.status = 'pending'
      AND r.remind_at <= pg_catalog.now()
  );
$$;

-- ---------------------------------------------------------------------------
-- Notification queue claims. Same model as pgmq (read counter +
-- visibility timeout), implemented in four statements so the queue
-- works on any Postgres rather than requiring the Supabase-only pgmq
-- extension. See 029_notify_jobs.sql.
--
-- read_ct increments on every claim, so the retry budget survives a
-- driver restart. A successful send deletes the row and never inflates
-- the count. A driver that dies after claiming leaves the row hidden
-- until vt expires, then it reappears.
--
-- p_vt is an interval, not seconds.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION private.claim_notifications(
  p_limit integer DEFAULT 20,
  p_vt    interval DEFAULT interval '60 seconds'
)
RETURNS SETOF jsonb
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
BEGIN
  RETURN QUERY
  WITH ready AS (
    SELECT j.id
    FROM public.notify_jobs j
    WHERE j.archived_at IS NULL
      AND j.vt <= pg_catalog.now()
    ORDER BY j.id
    LIMIT GREATEST(p_limit, 0)
    FOR UPDATE SKIP LOCKED
  ), claimed AS (
    UPDATE public.notify_jobs j
       SET read_ct = j.read_ct + 1,
           vt = pg_catalog.now() + p_vt
      FROM ready
     WHERE j.id = ready.id
    RETURNING j.id, j.to_phone, j.text, j.read_ct
  )
  SELECT jsonb_build_object(
    'id',      c.id,
    'to',      c.to_phone,
    'text',    c.text,
    'read_ct', c.read_ct
  )
  FROM claimed c;
END;
$$;

CREATE OR REPLACE FUNCTION private.delete_notification(p_id bigint)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
DECLARE
  v_n integer;
BEGIN
  DELETE FROM public.notify_jobs j WHERE j.id = p_id;
  GET DIAGNOSTICS v_n = ROW_COUNT;
  RETURN v_n > 0;
END;
$$;

-- Archive an exhausted job instead of dropping it, so failed
-- notifications stay inspectable. The in-process queue used to log and
-- discard after 3 attempts (queue.go:143).
CREATE OR REPLACE FUNCTION private.archive_notification(p_id bigint)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
DECLARE
  v_n integer;
BEGIN
  UPDATE public.notify_jobs j
     SET archived_at = pg_catalog.now()
   WHERE j.id = p_id
     AND j.archived_at IS NULL;
  GET DIAGNOSTICS v_n = ROW_COUNT;
  RETURN v_n > 0;
END;
$$;

CREATE OR REPLACE FUNCTION private.any_notifications_queued()
RETURNS boolean
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = ''
AS $$
  SELECT EXISTS (
    SELECT 1 FROM public.notify_jobs j
    WHERE j.archived_at IS NULL
      AND j.vt <= pg_catalog.now()
  );
$$;

-- ---------------------------------------------------------------------------
-- delivery_worker needs real table privileges. BYPASSRLS skips row
-- policies, not GRANTs: a SECURITY DEFINER function still runs its
-- owner's permissions. Without these the claim functions fail with
-- "permission denied for table" the first time they are called.
--
-- This is the entire blast radius of the Supabase delivery credential:
-- read pending reminders across tenants, flip their claim status, read
-- the bot ciphertext that owns each reminder, and move notification
-- jobs through their lifecycle. No client, usage, session, or payment
-- table. No plaintext token is ever returned.
-- ---------------------------------------------------------------------------
GRANT SELECT, UPDATE ON public.agent_reminders TO delivery_worker;
GRANT SELECT ON public.telegram_bots TO delivery_worker;
GRANT SELECT, UPDATE, DELETE ON public.notify_jobs TO delivery_worker;

-- ---------------------------------------------------------------------------
-- Grants. delivery_caller can execute these and nothing else. No USAGE
-- on public, no table grants, no BYPASSRLS.
--
-- OWNERSHIP IS THE POINT. Without the ALTER below these functions stay
-- owned by the migration runner (postgres, a superuser), and
-- SECURITY DEFINER would then execute as a superuser - bypassing every
-- RLS policy with far more authority than intended. Reassigning them to
-- delivery_worker pins definer rights to exactly BYPASSRLS, so the
-- Edge Function's database credential can do these two things and
-- nothing else.
--
-- reap_stale_claims is deliberately left owned by postgres: the Go boot
-- path calls it through the admin pool and does not need a definer.
-- ---------------------------------------------------------------------------
ALTER FUNCTION private.claim_due_reminders(integer)              OWNER TO delivery_worker;
ALTER FUNCTION private.complete_reminder(uuid, timestamptz, text)  OWNER TO delivery_worker;
ALTER FUNCTION private.fail_reminder(uuid, timestamptz, integer)   OWNER TO delivery_worker;
ALTER FUNCTION private.any_reminders_due()                        OWNER TO delivery_worker;
ALTER FUNCTION private.claim_notifications(integer, interval)     OWNER TO delivery_worker;
ALTER FUNCTION private.delete_notification(bigint)                OWNER TO delivery_worker;
ALTER FUNCTION private.archive_notification(bigint)               OWNER TO delivery_worker;
ALTER FUNCTION private.any_notifications_queued()                 OWNER TO delivery_worker;

GRANT EXECUTE ON FUNCTION
  private.claim_due_reminders(integer),
  private.complete_reminder(uuid, timestamptz, text),
  private.fail_reminder(uuid, timestamptz, integer),
  private.reap_stale_claims(interval),
  private.any_reminders_due(),
  private.claim_notifications(integer, interval),
  private.delete_notification(bigint),
  private.archive_notification(bigint),
  private.any_notifications_queued()
TO delivery_caller;

-- The Go boot path reaps stale claims on long-running hosts; the admin
-- pool reaches it directly, no definer required.
GRANT USAGE ON SCHEMA private TO app_runtime, app;
GRANT EXECUTE ON FUNCTION private.reap_stale_claims(interval) TO app_runtime, app;
