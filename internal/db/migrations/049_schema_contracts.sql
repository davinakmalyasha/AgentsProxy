-- 049_schema_contracts.sql — Phase D: per-tenant schema evolution without
-- physical separation. DDL stays global + additive (GOVERNANCE §3); rollout
-- becomes per-tenant and logical via contract rows.
--
-- tenant_schema_contracts: each tenant's feature versions. A canary bump is
-- a row update (admin endpoint), never a deploy. Rollback is a row update
-- back (handlers tolerate the previous version for one release).
-- feature_schema_versions: the versions that exist and what they need.
--
-- Additive only: two new tables, seed rows, no changes to existing tables.

CREATE TABLE IF NOT EXISTS tenant_schema_contracts (
  tenant_id  text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  feature    text NOT NULL,
  version    integer NOT NULL DEFAULT 1 CHECK (version >= 1),
  status     text NOT NULL DEFAULT 'ok' CHECK (status IN ('ok','failed')),
  applied_at timestamptz NOT NULL DEFAULT now(),
  error      text NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, feature)
);

CREATE TABLE IF NOT EXISTS feature_schema_versions (
  feature          text NOT NULL,
  version          integer NOT NULL CHECK (version >= 1),
  description      text NOT NULL DEFAULT '',
  requires_column  text NOT NULL DEFAULT '',
  PRIMARY KEY (feature, version)
);

-- Pilot versions (current shape = v1 for both).
INSERT INTO feature_schema_versions (feature, version, description, requires_column) VALUES
  ('bookings', 1, 'tombstone-ready ledger reads; idempotent writes', ''),
  ('pricing',  1, 'day/evening rules with open/close hours', '')
ON CONFLICT (feature, version) DO NOTHING;

ALTER TABLE tenant_schema_contracts ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_schema_contracts FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_schema_contracts_isolation ON tenant_schema_contracts;
CREATE POLICY tenant_schema_contracts_isolation ON tenant_schema_contracts
  FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE feature_schema_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE feature_schema_versions FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS feature_schema_versions_isolation ON feature_schema_versions;
-- Version catalog is global (no tenant_id column): every tenant reads all
-- rows, nobody writes via the runtime role.
CREATE POLICY feature_schema_versions_isolation ON feature_schema_versions
  FOR SELECT USING (true);

GRANT SELECT ON tenant_schema_contracts TO app_runtime;
GRANT SELECT ON feature_schema_versions TO app_runtime;
