-- 0012_staff_session_v2_clients.sql — restore the v2 resolver's clients read.
--
-- 0010 is the Centrum contract step for the retired schema-scanning auth
-- functions. It deliberately revokes db_schema and the blanket table SELECT
-- from auth_definer after the platform chain, leaving only tenant_id/status
-- for staff_session_create.
--
-- staff_session_resolve_v2 (platform migration 090) needs the routing and
-- entitlement columns to return a session's tenant without a second clients
-- query. On a fresh database the project chain runs after platform 090, so
-- 0010's revoke wins and v2 fails with "permission denied for table clients".
-- This project migration is the last word: grant exactly the columns v2
-- returns, no table-level access and no token/quota/PII columns.
--
-- Idempotent and additive.

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'auth_definer') THEN
    GRANT USAGE ON SCHEMA public TO auth_definer;
    GRANT SELECT (id, db_schema, status, vertical, features)
      ON public.clients TO auth_definer;
  END IF;
END $$;
