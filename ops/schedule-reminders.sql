-- ops/schedule-reminders.sql — pg_cron jobs for the PWA reminder path.
--
-- Installed 2026-10-15 on prod (extensions pg_cron 1.6.4 + pg_net 0.20.4
-- enabled first). Supersedes the reminder half of
-- ops/schedule-delivery.sql (whose reminder-tick line stays commented
-- for the pre-repair era; notify drain still lives there and is NOT
-- installed — deliver-notification was never deployed).
--
-- Jobs:
--   reminder-tick  */2 * * * *  probe-gated Edge invocation. The tick
--     function checks any_reminders_due() first: idle systems spend one
--     indexed query per tick and zero Edge invocations. No JWT needed —
--     deliver-reminders runs with verify_jwt=false (same posture as the
--     notify design; the claim protocol, not the URL, is the security
--     boundary — quarantine-aware claim, attempts ladder, reaper below).
--   reaper-tick    */5 * * * *  hands back claims orphaned by a crashed
--     driver (10-min window default: slow-but-alive drivers are never
--     robbed mid-send).
--
-- Behavior before secrets land (DELIVERY_DATABASE_URL / VAPID_* unset on
-- the function): ticks that find due rows POST into a 500 (missing DSN,
-- claimed nothing — the DSN check precedes the claim). Safe noise until
-- the env sweep; after secrets, first tick delivers. Secrets are live on
-- prod since 2026-10-18 (see ops/RUNBOOK-delivery.md); this paragraph
-- records the pre-secret transient only.
--
-- Removal (rollback / repair): SELECT cron.unschedule('reminder-tick');
-- Rows stay pending by construction — unscheduling loses nothing.

CREATE OR REPLACE FUNCTION private.tick_due_reminders()
RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
DECLARE
  v_due boolean;
BEGIN
  SELECT private.any_reminders_due() INTO v_due;
  IF NOT v_due THEN
    RETURN 0;
  END IF;
  PERFORM net.http_post(
    'https://hwcizjzkqexviegsyaql.supabase.co/functions/v1/deliver-reminders',
    '{}'::jsonb
  );
  RETURN 1;
END;
$$;
-- Ownership follows 031: driver-adjacent infra stays postgres-owned
-- (cron executes as the scheduling owner; no extra grants needed).

SELECT cron.schedule('reminder-tick', '*/2 * * * *',
  $$SELECT private.tick_due_reminders()$$);
SELECT cron.schedule('reaper-tick', '*/5 * * * *',
  $$SELECT private.reap_stale_claims()$$);
