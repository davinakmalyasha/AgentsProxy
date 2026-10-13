-- 057_push_reminders.sql — PWA Web Push delivery for reminders.
--
-- The Telegram transport is gone (042) and the reminder driver functions
-- were dropped as dead (055). This re-adds the driver half with the
-- quarantine-aware claim body (048 verbatim) plus the complete/fail
-- bodies (031 verbatim), and adds the push-subscription registry plus a
-- routable recipient on each reminder. chat_id stays for history but is
-- no longer the routing key: it is a legacy Telegram int64, while push
-- needs a subscription lookup.
--
-- Recipient model: agent_reminders.recipient_ref matches
-- push_subscriptions.user_ref exactly, scoped by tenant_id. The ref is
-- opaque (staff username, 'owner', a member id) — chosen at set_reminder
-- time, resolved at send time. No FK: subscriptions rotate (410 Gone)
-- independently of reminder rows.
--
-- Additive-only: one new table, one nullable-compatible column (DEFAULT
-- '' keeps old rows valid), three re-added functions. No existing rows
-- touched; nothing claims until a sender + schedule exist (a later step).

-- ---------------------------------------------------------------------------
-- §1. Push subscription registry.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS push_subscriptions (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  user_ref     text NOT NULL DEFAULT '',
  endpoint     text NOT NULL,
  p256dh       text NOT NULL DEFAULT '',
  auth         text NOT NULL DEFAULT '',
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz,
  UNIQUE (tenant_id, endpoint)
);

-- Sender lookup: all live subscriptions for one recipient.
CREATE INDEX IF NOT EXISTS push_subscriptions_recipient_idx
  ON public.push_subscriptions (tenant_id, user_ref);

ALTER TABLE push_subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE push_subscriptions FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS push_subscriptions_isolation ON push_subscriptions;
CREATE POLICY push_subscriptions_isolation ON push_subscriptions
  FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON push_subscriptions TO app_runtime;

-- ---------------------------------------------------------------------------
-- §2. Routable recipient on reminders.
-- ---------------------------------------------------------------------------

ALTER TABLE agent_reminders
  ADD COLUMN IF NOT EXISTS recipient_ref text NOT NULL DEFAULT '';

-- ---------------------------------------------------------------------------
-- §3. Driver functions (claim/complete/fail). Bodies verbatim from
-- 048 (quarantine-aware claim) and 031 (complete/fail); owners + grants
-- mirror 031 exactly (delivery_worker owns, delivery_caller executes).
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
    RETURNING r.id, r.tenant_id, r.chat_id, r.text, r.attempts
  )
  SELECT jsonb_build_object(
    'id',         c.id,
    'tenant_id',  c.tenant_id,
    'chat_id',    c.chat_id,
    'text',       c.text,
    'attempts',   c.attempts,
    'claimed_at', v_ts
  )
  FROM claimed c;
END;
$$;

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

ALTER FUNCTION private.claim_due_reminders(integer)              OWNER TO delivery_worker;
ALTER FUNCTION private.complete_reminder(uuid, timestamptz, text)  OWNER TO delivery_worker;
ALTER FUNCTION private.fail_reminder(uuid, timestamptz, integer)   OWNER TO delivery_worker;

GRANT EXECUTE ON FUNCTION
  private.claim_due_reminders(integer),
  private.complete_reminder(uuid, timestamptz, text),
  private.fail_reminder(uuid, timestamptz, integer)
TO delivery_caller;
