-- 064_report_jobs.sql — deferred (async) Excel report generation.
--
-- The export tool builds the workbook inside the agent turn. A large
-- range raises statement_timeout to 120s while the turn budget is ~80s
-- (adkrunner.TurnTimeout), so big exports could be killed mid-build with
-- no file and no retry. Deferred builds move that work to a
-- one-job-per-tick worker: the tool enqueues and returns a job link;
-- pg_cron posts to the backend worker endpoint; the link redirects to
-- /v1/reports/{token} once the file exists.
--
-- Access model:
--   app_runtime: SELECT (RLS tenant-scoped) + INSERT — enqueue only.
--   the worker: admin pool (cross-tenant claim, like the reminder
--     scheduler), so no table grants are required for it.
--   private.report_job_get(uuid): capability lookup for the public job
--     link. The random UUID is the secret, matching the
--     /v1/reports/{token} model; the function returns routing fields
--     only (never report content).
--
-- Status ladder mirrors reminders: attempts cap → dead; the tick reaps
-- stale 'working' rows (10-minute window).

CREATE TABLE IF NOT EXISTS public.report_jobs (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  from_date   date NOT NULL,
  to_date     date NOT NULL,
  kind        text NOT NULL DEFAULT 'semua',
  status      text NOT NULL DEFAULT 'pending'
              CHECK (status IN ('pending', 'working', 'done', 'error', 'dead')),
  attempts    integer NOT NULL DEFAULT 0,
  claimed_at  timestamptz,
  object_key  text NOT NULL DEFAULT '',
  filename    text NOT NULL DEFAULT '',
  size_bytes  integer NOT NULL DEFAULT 0,
  error       text NOT NULL DEFAULT '',
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS report_jobs_pending_idx
  ON public.report_jobs (created_at) WHERE status = 'pending';

ALTER TABLE public.report_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.report_jobs FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS report_jobs_isolation ON public.report_jobs;
CREATE POLICY report_jobs_isolation ON public.report_jobs
  FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

GRANT SELECT, INSERT ON public.report_jobs TO app_runtime;

-- Capability lookup for the public job link (no tenant GUC on that path).
CREATE OR REPLACE FUNCTION private.report_job_get(p_id uuid)
RETURNS TABLE(tenant_id text, status text, object_key text, filename text,
              size_bytes integer, error text)
LANGUAGE sql
SECURITY DEFINER
SET search_path = ''
AS $$
  SELECT j.tenant_id, j.status, j.object_key, j.filename, j.size_bytes, j.error
    FROM public.report_jobs j
   WHERE j.id = p_id;
$$;
ALTER FUNCTION private.report_job_get(uuid) OWNER TO postgres;
REVOKE ALL ON FUNCTION private.report_job_get(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION private.report_job_get(uuid) TO app_runtime;
