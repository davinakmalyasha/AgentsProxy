-- 029_notify_jobs.sql - durable notification queue.
-- DONE: Vercel/Supabase split (serverless-safe notify) / STANDARDS A, D, E
--
-- WHY: the notify queue used to be an in-process channel drained by a
-- worker goroutine. That is correct on a long-running host but silently
-- loses every job on a serverless runtime, where the handler returns 201
-- and the invocation (plus the worker) is destroyed before the send.
-- Persisting the job at enqueue time fixes it.
--
-- WHY NOT pgmq: pgmq is a Supabase-only extension and is not available
-- on Laragon's Postgres, a VPS, or anywhere else. Migrations run in one
-- transaction (migrate.go), so depending on it would abort the entire
-- migration chain on any host without it. This table is the same
-- model pgmq uses - a read counter plus a visibility timeout - in four
-- statements, and it works on any Postgres 14+.
--
-- Queue mechanics (identical to pgmq's, reimplemented in migration 031):
--   enqueue  INSERT
--   claim    UPDATE ... SET read_ct+1, vt = now() + interval, over rows
--            whose vt has expired, selected FOR UPDATE SKIP LOCKED
--   success  DELETE
--   exhausted UPDATE ... SET archived_at = now()
-- A driver that dies between claim and delete makes the row reappear
-- once vt expires, so nothing is lost and nothing is delivered twice
-- concurrently.
--
-- RLS: this is cross-tenant internal infrastructure like agent_reminders
-- (the notification carries the customer phone number but is not
-- tenant-scoped work). ENABLE + FORCE with the standard tenant policy;
-- the enqueue goes through the admin pool and the Supabase driver
-- through private.claim_notifications() owned by the BYPASSRLS
-- delivery_worker role.
--
-- Grant note (026): the runtime connection logs in as `app`, so both
-- roles are granted.
--
-- Idempotent.

CREATE TABLE IF NOT EXISTS notify_jobs (
  id          bigserial PRIMARY KEY,
  tenant_id   text NOT NULL,
  to_phone    text NOT NULL,
  text        text NOT NULL,
  read_ct     integer NOT NULL DEFAULT 0,
  vt          timestamptz NOT NULL DEFAULT now(),
  enqueued_at timestamptz NOT NULL DEFAULT now(),
  archived_at timestamptz
);

-- The claim query is "rows whose vt has expired, oldest first". A
-- partial index over live rows keeps it index-only; archived rows fall
-- out of the index entirely as the archive grows.
CREATE INDEX IF NOT EXISTS notify_jobs_ready_idx
  ON notify_jobs(vt)
  WHERE archived_at IS NULL;

-- Per-tenant inspection (support: "what did we tell this customer?").
CREATE INDEX IF NOT EXISTS notify_jobs_tenant_idx
  ON notify_jobs(tenant_id, enqueued_at DESC);

ALTER TABLE notify_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE notify_jobs FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS notify_jobs_isolation ON notify_jobs;
CREATE POLICY notify_jobs_isolation ON notify_jobs
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON notify_jobs TO app_runtime, app;
GRANT USAGE, SELECT ON SEQUENCE notify_jobs_id_seq TO app_runtime, app;
