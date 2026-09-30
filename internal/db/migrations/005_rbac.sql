-- 005_rbac.sql — Phase 3: production runtime role split.
-- DONE: PRD 6.5 (feature platform) / STANDARDS D, H
--
-- Production runtime connects as `app_runtime`:
--   * SELECT-only on `clients`  (cannot self-provision or change status)
--   * Full DML on `tenant_records` and `usage`
--   * NO DDL anywhere
--
-- Admin (migrations, /v1/admin/*) connects as the existing superuser
-- role via `DATABASE_ADMIN_URL`.
--
-- Single runtime role by design (Phase 1.2): `app_runtime` is the only
-- login role the migrations create. There is no `app` role — every
-- `TO app` grant was removed, because no migration creates that role
-- and an unguarded GRANT aborts the whole chain on a fresh database
-- (migrate.go runs all migrations in one transaction).

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_runtime') THEN
    CREATE ROLE app_runtime LOGIN;
  ELSE
    ALTER ROLE app_runtime LOGIN;
  END IF;
END$$;

GRANT USAGE ON SCHEMA public TO app_runtime;
GRANT SELECT ON clients TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_records TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON usage TO app_runtime;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_runtime;
