-- 033_clients_usage_rls.sql — RLS policies for the two tables that are
-- deliberately NOT tenant-scoped: the tenant registry (clients) and the
-- client quota ledger (usage).
--
-- Every other table gets the `tenant_id = current_setting('app.tenant_id')`
-- isolation policy. These two cannot, and that is by design:
--
--   clients is the REGISTRY. records.KnownTenant has to resolve a tenant
--   BEFORE any tenant context exists, so app.tenant_id is still NULL at
--   that moment and a tenant_id-based policy could never match a row.
--
--   usage is keyed by client_id (FK to clients.id) and has no tenant_id
--   column at all, so there is nothing for an isolation policy to test.
--
-- Both tables had RLS enabled out-of-band — no migration in this repo
-- enables it on either — while carrying zero policies. RLS with no
-- policies denies every row to every non-owner role, so app_runtime
-- (granted only SELECT on clients by 005_rbac.sql) saw zero rows while
-- the table owner postgres bypassed RLS and saw everything. Because
-- KnownTenant swallows query errors, the entire public landing surface
-- answered 400 "unknown tenant" for a tenant that plainly existed, and
-- quota accounting read 0 with its INSERT error discarded by `_ =`.
--
-- These policies are scoped TO app_runtime, which is the only non-owner
-- role holding any privilege on these tables: anon / authenticated /
-- service_role have only REFERENCES, TRIGGER and TRUNCATE, so a
-- permissive USING (true) here cannot leak through the Supabase public
-- API. INSERT/UPDATE/DELETE on clients remain denied for app_runtime,
-- which is exactly what internal/db/rbac_test.go asserts.
--
-- RLS is NOT forced on these two tables, unlike the tenant-scoped ones:
-- the owner must stay able to write them so tenants can be provisioned
-- through the admin API.

-- Tenant registry. The runtime role must read it to validate a tenant;
-- it is read-only, since provisioning goes through the admin pool.
DROP POLICY IF EXISTS clients_read_runtime ON public.clients;
CREATE POLICY clients_read_runtime ON public.clients
  FOR SELECT
  TO app_runtime
  USING (true);

-- Quota ledger. quota.Check reads it and usage.Record appends to it, both
-- on the runtime pool.
DROP POLICY IF EXISTS usage_read_runtime ON public.usage;
CREATE POLICY usage_read_runtime ON public.usage
  FOR SELECT
  TO app_runtime
  USING (true);

DROP POLICY IF EXISTS usage_write_runtime ON public.usage;
CREATE POLICY usage_write_runtime ON public.usage
  FOR INSERT
  TO app_runtime
  WITH CHECK (true);
