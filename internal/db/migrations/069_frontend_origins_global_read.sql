-- 069_frontend_origins_global_read.sql — registry reads are global.
--
-- Bug: 058 scoped ALL commands on frontend_origins to
-- tenant_id = current_setting('app.tenant_id'). The CORS resolver
-- (middleware.DBResolver) reads whole-set on a connection with no GUC,
-- so as app_runtime it loaded ZERO exact rows in production — every
-- exact origin silently depended on the ALLOW_ORIGINS env var, which is
-- why origins "disappeared" on every update. Suffix rules were
-- unaffected (global USING (true) policy).
--
-- Fix: SELECT is global (mirrors origin_suffix_rules; the runtime role
-- holds only the SELECT grant anyway). Writes stay tenant-scoped so a
-- widened grant can never cross-write. Admin writes go through the
-- admin pool (superuser bypasses RLS regardless).
-- Additive-only: policies replaced, no rows touched.

DROP POLICY IF EXISTS frontend_origins_isolation ON frontend_origins;

CREATE POLICY frontend_origins_global_select ON frontend_origins
  FOR SELECT USING (true);

CREATE POLICY frontend_origins_tenant_insert ON frontend_origins
  FOR INSERT WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY frontend_origins_tenant_update ON frontend_origins
  FOR UPDATE USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY frontend_origins_tenant_delete ON frontend_origins
  FOR DELETE USING (tenant_id = current_setting('app.tenant_id', true));
