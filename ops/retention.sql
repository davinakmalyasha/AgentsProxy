-- retention.sql - one-time environment setup for Supabase-hosted
-- retention (pg_cron). Mirrors ops/schedule-delivery.sql's contract:
-- deliberately NOT a migration (references pg_cron, which only exists
-- on the hosted project, and schedules by wall-clock).
--
-- WHY: the Go janitor in cmd/server/main.go only runs on long-lived
-- hosts and only prunes 3 tables. On Vercel (BACKGROUND_TICKERS=false)
-- nothing is ever deleted, and these tables grow forever:
--   usage               one row per LLM call, every tenant (biggest table)
--   events/sessions     ADK transcripts, never pruned by any janitor
--   notify_jobs         archived rows kept forever
--   report_objects      sweeper misses (backstop only; the sweeper owns it)
--   tombstoned ledgers  preserved by design (046/051) — NOT purged here
--
-- Run ONCE per environment, AFTER migrations have been applied:
--
--   psql "$DATABASE_ADMIN_URL" -f ops/retention.sql
--
-- Local dev (Laragon) does not need this: the Go janitor owns the 3
-- small tables there. Keep the Go janitor as the local fallback.
--
-- Installed on prod 2026-10-17: job retention-tick active (17 3 * * *);
-- manual tick proof ran all 7 targets (0 deleted on the still-empty
-- dataset). First-run fix lives in §2 (unschedule by jobid lookup).

-- ---------------------------------------------------------------------------
-- 1. The tick. Batched deletes: an unbounded DELETE on a large table
--    holds locks for its whole duration and emits one huge WAL spike.
--    ctid batches of 5000 keep each statement short; the loop continues
--    until a batch comes back short.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION private.retention_tick()
RETURNS TABLE(table_name text, deleted bigint)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = '' AS $$
DECLARE
  v_n bigint;
  v_batch bigint;
BEGIN
  DELETE FROM public.sports_sessions
   WHERE expires_at < pg_catalog.now() - interval '7 days';
  GET DIAGNOSTICS v_n = ROW_COUNT;
  table_name := 'sports_sessions'; deleted := v_n; RETURN NEXT;

  DELETE FROM public.agent_reminders
   WHERE status = 'sent' AND created_at < pg_catalog.now() - interval '90 days';
  GET DIAGNOSTICS v_n = ROW_COUNT;
  table_name := 'agent_reminders_sent'; deleted := v_n; RETURN NEXT;

  DELETE FROM public.agent_reminders
   WHERE status = 'dead' AND created_at < pg_catalog.now() - interval '30 days';
  GET DIAGNOSTICS v_n = ROW_COUNT;
  table_name := 'agent_reminders_dead'; deleted := v_n; RETURN NEXT;

  -- usage: batched, this is the biggest table in the system.
  v_n := 0;
  LOOP
    DELETE FROM public.usage WHERE ctid IN (
      SELECT ctid FROM public.usage
       WHERE created_at < pg_catalog.now() - interval '400 days'
       LIMIT 5000);
    GET DIAGNOSTICS v_batch = ROW_COUNT;
    v_n := v_n + v_batch;
    EXIT WHEN v_batch < 5000;
  END LOOP;
  table_name := 'usage'; deleted := v_n; RETURN NEXT;

  DELETE FROM public.notify_jobs
   WHERE archived_at IS NOT NULL
     AND archived_at < pg_catalog.now() - interval '14 days';
  GET DIAGNOSTICS v_n = ROW_COUNT;
  table_name := 'notify_jobs'; deleted := v_n; RETURN NEXT;

  -- events: batched like usage. 180 days keeps half a year of agent
  -- transcripts; tighten per product decision, never silently.
  v_n := 0;
  LOOP
    DELETE FROM public.events WHERE ctid IN (
      SELECT ctid FROM public.events
       WHERE "timestamp" < pg_catalog.now() - interval '180 days'
       LIMIT 5000);
    GET DIAGNOSTICS v_batch = ROW_COUNT;
    v_n := v_n + v_batch;
    EXIT WHEN v_batch < 5000;
  END LOOP;
  table_name := 'events'; deleted := v_n; RETURN NEXT;

  -- Backstop only: the Go sweeper owns expired manifests. This catches
  -- rows the sweeper missed (e.g. RLS-blind periods pre-053).
  DELETE FROM public.report_objects
   WHERE expires_at < pg_catalog.now() - interval '1 day';
  GET DIAGNOSTICS v_n = ROW_COUNT;
  table_name := 'report_objects'; deleted := v_n; RETURN NEXT;

  RETURN;
END;
$$;
ALTER FUNCTION private.retention_tick() OWNER TO postgres;
REVOKE ALL ON FUNCTION private.retention_tick() FROM PUBLIC;

-- ---------------------------------------------------------------------------
-- 2. Schedule. Remove first so re-running is idempotent. Unschedule-by-
--    lookup, not cron.unschedule('name'): the name form RAISES on a
--    missing job (XX000 "could not find valid entry"), which would make
--    the first run fail; the jobid form over zero rows is a no-op. 03:17
--    local — off-peak, and off the :00/:05 marks every other job uses.
-- ---------------------------------------------------------------------------
SELECT cron.unschedule(jobid) FROM cron.job WHERE jobname = 'retention-tick';
SELECT cron.schedule('retention-tick', '17 3 * * *', $$SELECT private.retention_tick()$$);

-- ---------------------------------------------------------------------------
-- 3. Show what got installed.
-- ---------------------------------------------------------------------------
SELECT jobname, schedule, active FROM cron.job WHERE jobname = 'retention-tick';
