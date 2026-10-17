-- 059_reminder_push_rpcs.sql — Edge reminder driver gets RPC-only DB access.
--
-- The deployed deliver-reminders Edge Function runs as delivery_caller:
-- EXECUTE on the driver functions, USAGE on private, and NO table grants
-- (057's least-privilege split). Its first version read agent_reminders
-- and push_subscriptions directly, which would 42501 the moment the
-- claim connected — every row touch must go through an owner-scoped
-- SECURITY DEFINER function. This migration adds those:
--
--   claim_due_reminders v2  driver payload now carries recipient_ref, so
--                           routing needs no follow-up table read.
--   push_subscriptions_for  live subscriptions for (tenant, ref); mirrors
--                           the Go PushSender's lookup.
--   push_subscription_touch stamps last_used_at on a 2xx send.
--   push_subscription_gone  prunes a 404/410 endpoint.
--   release_reminder        hands a claim back to pending WITHOUT bumping
--                           attempts; mirrors Go Scheduler.releaseClaim
--                           (missing addressability is not a failure).
--
-- delivery_worker (NOLOGIN BYPASSRLS, owns the functions) gains exactly
-- the push_subscriptions privileges the helpers execute under: SELECT
-- (lookup), UPDATE (touch), DELETE (gone). PUBLIC execute is revoked
-- everywhere — the 031 lesson (default grants leaked the claim to
-- app_runtime until 057 closed it).
--
-- Additive-only: one payload key, five functions, one table grant.

-- ---------------------------------------------------------------------------
-- §1. Claim payload gains recipient_ref (routing without a table read).
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
     AND NOT EXISTS (SELECT 1 FROM public.tenant_quarantine q
                      WHERE q.tenant_id = r.tenant_id)
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
    RETURNING r.id, r.tenant_id, r.chat_id, r.text, r.attempts, r.recipient_ref
  )
  SELECT jsonb_build_object(
    'id',            c.id,
    'tenant_id',     c.tenant_id,
    'chat_id',       c.chat_id,
    'text',          c.text,
    'attempts',      c.attempts,
    'recipient_ref', c.recipient_ref,
    'claimed_at',    v_ts
  )
  FROM claimed c;
END;
$$;

-- ---------------------------------------------------------------------------
-- §2. Push-subscription helpers + claim release.
-- ---------------------------------------------------------------------------

-- Empty ref guard: recipient_ref = '' is the pre-push default and is
-- never routable; without the guard a legacy subscription row with
-- user_ref = '' could match it.
CREATE OR REPLACE FUNCTION private.push_subscriptions_for(
  p_tenant_id text,
  p_ref       text
)
RETURNS TABLE (endpoint text, p256dh text, auth text)
LANGUAGE sql
SECURITY DEFINER
SET search_path = ''
AS $$
  SELECT s.endpoint, s.p256dh, s.auth
  FROM public.push_subscriptions s
  WHERE s.tenant_id = p_tenant_id
    AND s.user_ref = p_ref
    AND p_ref <> ''
  ORDER BY s.endpoint;
$$;

CREATE OR REPLACE FUNCTION private.push_subscription_touch(
  p_tenant_id text,
  p_endpoint  text
)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
DECLARE
  v_n integer;
BEGIN
  UPDATE public.push_subscriptions s
     SET last_used_at = pg_catalog.now()
   WHERE s.tenant_id = p_tenant_id
     AND s.endpoint = p_endpoint;
  GET DIAGNOSTICS v_n = ROW_COUNT;
  RETURN v_n > 0;
END;
$$;

CREATE OR REPLACE FUNCTION private.push_subscription_gone(
  p_tenant_id text,
  p_endpoint  text
)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
DECLARE
  v_n integer;
BEGIN
  DELETE FROM public.push_subscriptions s
   WHERE s.tenant_id = p_tenant_id
     AND s.endpoint = p_endpoint;
  GET DIAGNOSTICS v_n = ROW_COUNT;
  RETURN v_n > 0;
END;
$$;

CREATE OR REPLACE FUNCTION private.release_reminder(
  p_id         uuid,
  p_claimed_at timestamptz
)
RETURNS boolean
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
   WHERE r.id = p_id
     AND r.status = 'sending'
     AND r.claimed_at = p_claimed_at;
  GET DIAGNOSTICS v_n = ROW_COUNT;
  RETURN v_n > 0;
END;
$$;

-- ---------------------------------------------------------------------------
-- §3. Ownership, least privilege, table grants for the helper owner.
-- ---------------------------------------------------------------------------

ALTER FUNCTION private.claim_due_reminders(integer)         OWNER TO delivery_worker;
ALTER FUNCTION private.push_subscriptions_for(text, text)   OWNER TO delivery_worker;
ALTER FUNCTION private.push_subscription_touch(text, text)  OWNER TO delivery_worker;
ALTER FUNCTION private.push_subscription_gone(text, text)   OWNER TO delivery_worker;
ALTER FUNCTION private.release_reminder(uuid, timestamptz)  OWNER TO delivery_worker;

-- The helpers run as delivery_worker: BYPASSRLS skips the tenant GUC
-- policy, but privilege checks still apply — without these grants the
-- definer body would 42501 on its own table.
GRANT SELECT, UPDATE, DELETE ON public.push_subscriptions TO delivery_worker;

REVOKE ALL ON FUNCTION private.claim_due_reminders(integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION private.push_subscriptions_for(text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION private.push_subscription_touch(text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION private.push_subscription_gone(text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION private.release_reminder(uuid, timestamptz) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION
  private.claim_due_reminders(integer),
  private.push_subscriptions_for(text, text),
  private.push_subscription_touch(text, text),
  private.push_subscription_gone(text, text),
  private.release_reminder(uuid, timestamptz)
TO delivery_caller;
