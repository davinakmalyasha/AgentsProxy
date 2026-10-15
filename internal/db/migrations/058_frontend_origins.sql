-- 058_frontend_origins.sql — CORS origin registry for N frontends.
--
-- One backend serves many frontend deployments (one per project), each
-- with its own production domain. An env allowlist does not scale past
-- a handful of origins (one edit + redeploy per onboarding) and cannot
-- express per-tenant scoping. Origins become tenant data: one INSERT
-- onboards a frontend, zero deploys, audited by created_by.
--
-- Matching (internal/middleware/origins.go):
--   exact rows: full origin, byte-compared after normalization
--     (trim spaces, strip one trailing slash, lowercase).
--   suffix: rules stored as '.example.com' match any subdomain with a
--     dot boundary (evil-example.com does NOT match). Suffix rules are
--     global (no tenant column) like feature_schema_versions (049):
--     every tenant reads them, only the admin path writes.
-- `*` is never a valid row: validated on write (admin endpoint) and
-- rejected at load. Local dev keeps working via the ALLOW_ORIGINS env
-- list (merged with this registry for one release; see router.go).
--
-- Cutover: seeds mirror the pre-registry env value exactly, so day-one
-- behavior is identical. The seed inserts only where the client row
-- exists (fresh/dev databases seed nothing; dev runs ENV<>production
-- with the env list, production without rows AND without env origins
-- fails boot closed — that misconfiguration is the point).
--
-- Additive-only: two new tables, grants, seeds. No existing rows touched.

CREATE TABLE IF NOT EXISTS frontend_origins (
  tenant_id  text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  origin     text NOT NULL,
  label      text NOT NULL DEFAULT '',
  created_by text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, origin)
);

CREATE TABLE IF NOT EXISTS origin_suffix_rules (
  suffix     text PRIMARY KEY,
  note       text NOT NULL DEFAULT '',
  created_by text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now()
);

-- Sender lookup is whole-set (boot + 60s refresh), single round trip.
-- The recipient index serves future per-tenant inspection endpoints.
CREATE INDEX IF NOT EXISTS frontend_origins_tenant_idx
  ON public.frontend_origins (tenant_id);

ALTER TABLE frontend_origins ENABLE ROW LEVEL SECURITY;
ALTER TABLE frontend_origins FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS frontend_origins_isolation ON frontend_origins;
CREATE POLICY frontend_origins_isolation ON frontend_origins
  FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE origin_suffix_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE origin_suffix_rules FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS origin_suffix_rules_isolation ON origin_suffix_rules;
-- Suffix rules are global (no tenant_id column): every tenant reads all
-- rows, nobody writes via the runtime role.
CREATE POLICY origin_suffix_rules_isolation ON origin_suffix_rules
  FOR SELECT USING (true);

-- The resolver runs as app_runtime (pool role): reads the registry,
-- writes nothing. Admin writes go through the admin pool.
GRANT SELECT ON frontend_origins TO app_runtime;
GRANT SELECT ON origin_suffix_rules TO app_runtime;

-- Day-one seeds mirror the pre-registry env value. Insert only where
-- the client exists: no failure on databases without that tenant, and
-- no orphan rows ever (FK would reject them anyway).
INSERT INTO frontend_origins (tenant_id, origin, label, created_by)
SELECT 'centrum', 'https://centrum-badminton.vercel.app', 'centrum prod frontend', 'migration-058'
WHERE EXISTS (SELECT 1 FROM clients WHERE tenant_id = 'centrum')
ON CONFLICT (tenant_id, origin) DO NOTHING;
