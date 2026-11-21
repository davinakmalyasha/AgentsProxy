-- 0002_tenant_records.sql — restore the always-on records table.
--
-- The project router mounts /records unconditionally, but Centrum's schema
-- was built from custom sports tables and never received tenant_records.
-- A valid client token calling /v1/p/centrum/records therefore hit a
-- missing relation. public.tenant_records is empty, so there is no data to
-- move; this creates the same contract inside project_centrum.
--
-- Keep this table in every project schema: records is the core feature that
-- is available before a project opts into anything else.

CREATE TABLE IF NOT EXISTS tenant_records (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id  text NOT NULL REFERENCES public.clients(tenant_id) ON DELETE CASCADE,
  vertical   text NOT NULL,
  kind       text NOT NULL,
  title      text NOT NULL DEFAULT '',
  slug       text NOT NULL DEFAULT '',
  payload    jsonb NOT NULL DEFAULT '{}'::jsonb,
  status     text NOT NULL DEFAULT 'active',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS tenant_records_tenant_vertical_kind_idx
  ON tenant_records (tenant_id, vertical, kind);
CREATE INDEX IF NOT EXISTS tenant_records_slug_idx
  ON tenant_records (slug);
CREATE INDEX IF NOT EXISTS tenant_records_payload_gin_idx
  ON tenant_records USING GIN (payload);
CREATE INDEX IF NOT EXISTS tenant_records_list_idx
  ON tenant_records (tenant_id, vertical, status, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS tenant_records_kind_list_idx
  ON tenant_records (tenant_id, vertical, kind, status, created_at DESC, id DESC);

ALTER TABLE tenant_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_records FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_records_isolation ON tenant_records;
CREATE POLICY tenant_records_isolation ON tenant_records
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- auto_grant is false for Centrum so its grants stay deliberate.
GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_records TO app_runtime;
