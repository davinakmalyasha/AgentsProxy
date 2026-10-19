-- schedule-report-jobs.sql — pg_cron worker for deferred report builds
-- (migration 064). One job per minute, probe-gated like the reminder
-- tick: an idle system spends one indexed query per tick and zero HTTP.
--
-- Setup per environment (once):
--   select vault.create_secret('https://agents-proxy-nine.vercel.app',
--                              'report_worker_url');
--   select vault.create_secret('<REPORT_WORKER_TOKEN>', 'report_worker_token');
-- The token must match the backend env REPORT_WORKER_TOKEN. While either
-- secret is missing the tick is a no-op (RETURN 0) — never an open call.
--
-- The tick also reaps 'working' jobs orphaned by a crashed worker
-- (10-minute window) back to pending, mirroring reap_stale_claims.

CREATE OR REPLACE FUNCTION private.tick_report_jobs()
RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
DECLARE
  v_url text;
  v_tok text;
BEGIN
  UPDATE public.report_jobs
     SET status = 'pending', claimed_at = NULL, updated_at = pg_catalog.now()
   WHERE status = 'working'
     AND claimed_at < pg_catalog.now() - interval '10 minutes';

  IF NOT EXISTS (SELECT 1 FROM public.report_jobs WHERE status = 'pending') THEN
    RETURN 0;
  END IF;

  SELECT value INTO v_url FROM vault.decrypted_secrets WHERE name = 'report_worker_url';
  SELECT value INTO v_tok FROM vault.decrypted_secrets WHERE name = 'report_worker_token';
  IF v_url IS NULL OR v_tok IS NULL OR v_url = '' OR v_tok = '' THEN
    RETURN 0;
  END IF;

  PERFORM net.http_post(
    v_url || '/v1/internal/report-jobs/build',
    '{}'::jsonb,
    '{}'::jsonb,
    pg_catalog.jsonb_build_object(
      'Content-Type', 'application/json',
      'X-Report-Worker-Token', v_tok
    ),
    150000
  );
  RETURN 1;
END;
$$;
ALTER FUNCTION private.tick_report_jobs() OWNER TO postgres;
REVOKE ALL ON FUNCTION private.tick_report_jobs() FROM PUBLIC;

SELECT cron.unschedule(jobid) FROM cron.job WHERE jobname = 'report-jobs-tick';
SELECT cron.schedule('report-jobs-tick', '* * * * *', $$SELECT private.tick_report_jobs()$$);

SELECT jobname, schedule, active FROM cron.job WHERE jobname = 'report-jobs-tick';
