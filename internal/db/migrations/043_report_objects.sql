-- 043_report_objects.sql — durable report manifest for Supabase Storage.
--
-- Download tickets stay in Redis (hot, 30 min TTL). This manifest is the
-- sweeper cursor + audit trail: token hash (never the raw token), tenant,
-- storage object key, size, and expiry. The sweeper deletes expired
-- objects from the active ReportStorage backend, then the row.

CREATE TABLE IF NOT EXISTS report_objects (
  token_hash text PRIMARY KEY,
  tenant_id  text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  object_key text NOT NULL UNIQUE,
  size_bytes integer NOT NULL CHECK (size_bytes > 0),
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL DEFAULT now() + make_interval(secs => 1800)
);

CREATE INDEX IF NOT EXISTS report_objects_expires_idx ON report_objects(expires_at);
CREATE INDEX IF NOT EXISTS report_objects_tenant_idx ON report_objects(tenant_id);

ALTER TABLE report_objects ENABLE ROW LEVEL SECURITY;
ALTER TABLE report_objects FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS report_objects_isolation ON report_objects;
CREATE POLICY report_objects_isolation ON report_objects
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON report_objects TO app_runtime;
