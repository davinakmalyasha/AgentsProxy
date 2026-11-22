-- schedule-delivery.sql — HISTORICAL, DO NOT RUN.
--
-- This file installed a Database Webhook for `deliver-notification`, an
-- Edge Function that is not deployed. Running it unschedules the working
-- reminder tick and installs a broken notification path. The current
-- delivery matrix is in ops/RUNBOOK-delivery.md: reminders use
-- `deliver-reminders`; the notify drain is not installed on Vercel.
--
-- It is kept only as a record of the pre-push design. It is deliberately
-- not a migration and is not referenced by any runbook.
--
-- DONE: Vercel/Supabase split / STANDARDS A, E

\set ON_ERROR_STOP on

-- ---------------------------------------------------------------------------
-- 0. Extensions. pg_cron and pg_net are managed by Supabase; on vanilla
-- Postgres (Laragon, a VPS) this script simply does not apply.
-- ---------------------------------------------------------------------------
CREATE EXTENSION IF NOT EXISTS pg_cron;
CREATE EXTENSION IF NOT EXISTS pg_net;

-- ---------------------------------------------------------------------------
-- 1. Prereq check. Fail loudly rather than installing a job that can
--    never succeed.
-- ---------------------------------------------------------------------------
DO $$
DECLARE
  v_fn_url text;
BEGIN
  SELECT decrypted_secret INTO v_fn_url
    FROM vault.decrypted_secrets WHERE name = 'delivery_functions_url';
  IF v_fn_url IS NULL THEN
    RAISE EXCEPTION
      'vault secret delivery_functions_url is missing. Create it first:
         select vault.create_secret(''<project-ref>.functions.supabase.co'', ''delivery_functions_url'');
       Then re-run. See ops/RUNBOOK-delivery.md.';
  END IF;
END $$;

-- ---------------------------------------------------------------------------
-- 2. Reminder tick — DO NOT SCHEDULE (verified 2026-10-11: prod has no
--    pg_cron at all, so nothing is burning; keep it that way).
--
--    The `deliver-reminders` Edge Function is deleted (see 042_drop_telegram.sql).
--    private.tick_due_reminders() below still posts to '/deliver-reminders',
--    so scheduling `reminder-tick` would burn a failed pg_net call every 2 min.
--    Do NOT run the reminder-tick schedule below until a replacement delivery
--    target lands — then uncomment it and re-run this file (it uses
--    CREATE OR REPLACE + cron.schedule with IF NOT EXISTS semantics —
--    check the job block below).
-- ---------------------------------------------------------------------------
--    The exists() gate is the whole point of this being a function
--    rather than a bare net.http_post in the cron command: a bare post
--    would spin the Edge Function 720 times a day to discover there is
--    nothing to send. Gated, it costs one indexed probe and only calls
--    out when a reminder is actually due, which on the Supabase free
--    tier (500k invocations/month) is the difference between ~10% of
--    quota and a rounding error.
--
--    Every 2 minutes rather than every minute: a reminder set for 8:00
--    arriving at 8:02 is still on time for a human, and it halves the
--    probe count. Tighten to * * * * * if a tenant ever needs it.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION private.tick_due_reminders()
RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
DECLARE
  v_url   text;
  v_token text;
  v_id    bigint;
BEGIN
  IF NOT private.any_reminders_due() THEN
    RETURN 0;
  END IF;
  SELECT decrypted_secret INTO v_url
    FROM vault.decrypted_secrets WHERE name = 'delivery_functions_url';
  SELECT decrypted_secret INTO v_token
    FROM vault.decrypted_secrets WHERE name = 'delivery_auth_token';

  v_id := net.http_post(
    -- PENDING-REPAIR: '/deliver-reminders' function is deleted; kept until repair
    url     := v_url || '/deliver-reminders',
    headers := jsonb_build_object(
                 'Content-Type',  'application/json',
                 'Authorization', 'Bearer ' || v_token),
    timeout_milliseconds := 10000
  );
  RETURN v_id;
END;
$$;

-- Reclaim claims orphaned by a driver that crashed mid-send. Runs every
-- 5 minutes; the window is 10 minutes, so a slow-but-alive driver is
-- never robbed mid-send.
--
-- No Edge Function and no HTTP involved: reap_stale_claims() is pure
-- SQL, so pg_cron calls it directly. Routing this through a function
-- would spend an Edge Function invocation every 5 minutes to perform a
-- single indexed UPDATE.
CREATE OR REPLACE FUNCTION private.reap_claims_tick()
RETURNS integer
LANGUAGE sql
SECURITY DEFINER
SET search_path = ''
AS $$
  SELECT private.reap_stale_claims(interval '10 minutes');
$$;

-- ---------------------------------------------------------------------------
-- 3. Notification backstop. The Database Webhook below handles the
--    instant path; this exists because pg_net is best-effort, so a
--    dropped webhook call would otherwise strand a job until the next
--    booking.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION private.drain_notifications_tick()
RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
DECLARE
  v_url   text;
  v_token text;
  v_id    bigint;
BEGIN
  IF NOT private.any_notifications_queued() THEN
    RETURN 0;
  END IF;
  SELECT decrypted_secret INTO v_url
    FROM vault.decrypted_secrets WHERE name = 'delivery_functions_url';
  SELECT decrypted_secret INTO v_token
    FROM vault.decrypted_secrets WHERE name = 'delivery_auth_token';

  v_id := net.http_post(
    url     := v_url || '/deliver-notification',
    headers := jsonb_build_object(
                 'Content-Type',  'application/json',
                 'Authorization', 'Bearer ' || v_token),
    timeout_milliseconds := 10000
  );
  RETURN v_id;
END;
$$;

-- ---------------------------------------------------------------------------
-- 4. Register the cron jobs. unschedule first so re-running this
--    script is idempotent.
-- ---------------------------------------------------------------------------
DO $$
DECLARE
  j text;
BEGIN
  FOR j IN SELECT unnest(ARRAY['reminder-tick', 'reaper-tick', 'notify-drain'])
  LOOP
    PERFORM cron.unschedule(j);
  END LOOP;

  -- DO NOT SCHEDULE (verified 2026-10-11): `reminder-tick` posts to the
  -- deleted '/deliver-reminders' function (see private.tick_due_reminders
  -- below). Uncomment the reminder-tick line and re-run this file only
  -- when a replacement delivery target lands.
  -- PERFORM cron.schedule('reminder-tick', '*/2 * * * *',
  --   $$SELECT private.tick_due_reminders()$$);
  PERFORM cron.schedule('reaper-tick', '*/5 * * * *',
    $$SELECT private.reap_claims_tick()$$);
  PERFORM cron.schedule('notify-drain', '*/5 * * * *',
    $$SELECT private.drain_notifications_tick()$$);
END $$;

-- ---------------------------------------------------------------------------
-- 5. Instant delivery for new notification jobs.
--
--    INSERT-only on purpose. The claim path UPDATEs rows (read_ct, vt)
--    and the completion path DELETEs or archives them; hooking those
--    too would make every drain re-trigger itself forever.
--
--    Replaces the trigger first so re-running does not stack copies.
-- ---------------------------------------------------------------------------
DROP TRIGGER IF EXISTS notify_jobs_deliver ON public.notify_jobs;

CREATE TRIGGER notify_jobs_deliver
AFTER INSERT ON public.notify_jobs
FOR EACH ROW
EXECUTE FUNCTION supabase_functions.http_request(
  (SELECT decrypted_secret FROM vault.decrypted_secrets
    WHERE name = 'delivery_functions_url') || '/deliver-notification',
  'POST',
  jsonb_build_object(
    'Content-Type',  'application/json',
    'Authorization', 'Bearer ' || (SELECT decrypted_secret
      FROM vault.decrypted_secrets WHERE name = 'delivery_auth_token')),
  '{}'::jsonb,
  '10000'
);

-- ---------------------------------------------------------------------------
-- 6. Show what got installed.
-- ---------------------------------------------------------------------------
SELECT jobname, schedule, active FROM cron.job
 WHERE jobname IN ('reminder-tick', 'reaper-tick', 'notify-drain')
 ORDER BY jobname;
