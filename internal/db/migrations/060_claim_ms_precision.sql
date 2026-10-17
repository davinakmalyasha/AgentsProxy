-- 060_claim_ms_precision.sql — Edge-safe claimed_at precision on the RPC claim.
--
-- The Edge driver binds the claim payload's claimed_at string with
-- postgres.js, which truncates ISO timestamps to MILLISECOND precision
-- on the way in (scratch-verified: payload "2026-10-17T16:25:50.627081+07:00"
-- arrives as "2026-10-17 16:25:50.627+07"). The driver RPCs compare
-- claimed_at for claim-scoped updates, so a microsecond claim could
-- never be released/completed/failed from the Edge path — held rows
-- stuck in 'sending' until the reaper, sent rows re-delivered after it.
--
-- Fix at the source: the RPC claim stamps millisecond precision, so the
-- payload round-trips exactly through any client. The Go driver owns its
-- own claim SQL (raw microsecond clock_timestamp, native time.Time bind)
-- and is unaffected either way; both drivers only read status='pending',
-- so mixed precision across drivers cannot collide (the reaper window is
-- ten minutes, far beyond any rounding).
--
-- Additive-only: function replace + re-asserted owner/grants.

CREATE OR REPLACE FUNCTION private.claim_due_reminders(p_limit integer DEFAULT 20)
RETURNS SETOF jsonb
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
DECLARE
  v_ids  uuid[];
  v_ts   timestamptz := pg_catalog.date_trunc('milliseconds',
                                             pg_catalog.clock_timestamp());
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

ALTER FUNCTION private.claim_due_reminders(integer) OWNER TO delivery_worker;

REVOKE ALL ON FUNCTION private.claim_due_reminders(integer) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION private.claim_due_reminders(integer) TO delivery_caller;
