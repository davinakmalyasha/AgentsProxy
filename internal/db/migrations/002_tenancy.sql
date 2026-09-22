-- 002_tenancy.sql — monolith table for all 12 verticals.
-- One Supabase Postgres, many tenants. RLS + tenant_id is the
-- security boundary; the Go handler also runs SET LOCAL app.tenant_id
-- before every DML as defense-in-depth.

-- DONE: PRD 4.2 (RLS PERMISSIVE; see PRD comment for why not RESTRICTIVE)
CREATE TABLE IF NOT EXISTS tenant_records (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  vertical      text NOT NULL,
  kind          text NOT NULL,
  title         text NOT NULL DEFAULT '',
  slug          text NOT NULL DEFAULT '',
  payload       jsonb NOT NULL DEFAULT '{}'::jsonb,
  status        text NOT NULL DEFAULT 'active',
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS tenant_records_tenant_vertical_kind_idx
  ON tenant_records(tenant_id, vertical, kind);
CREATE INDEX IF NOT EXISTS tenant_records_slug_idx ON tenant_records(slug);
CREATE INDEX IF NOT EXISTS tenant_records_payload_gin_idx
  ON tenant_records USING GIN (payload);

ALTER TABLE tenant_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_records FORCE ROW LEVEL SECURITY;

-- PERMISSIVE policy: only rows where tenant_id matches the
-- per-connection `app.tenant_id` GUC are visible / writable. The Go
-- handler sets this GUC at the start of every transaction via
-- `set_config(..., is_local=true)` (defense-in-depth even if a
-- future handler forgets the WHERE).
-- Note: PERMISSIVE (not RESTRICTIVE) because a RESTRICTIVE-only
-- policy would deny everything (no permissive policy to allow it).
DROP POLICY IF EXISTS tenant_isolation ON tenant_records;
CREATE POLICY tenant_isolation ON tenant_records
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
