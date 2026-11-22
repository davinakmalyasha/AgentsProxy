-- 0010_contract_legacy_auth.sql — the platform owns auth state, not project users.
--
-- 081 added staff_session_create / staff_session_resolve and the provider
-- registry moved Centrum identity into projects/centrum/backend/auth. The old
-- schema-scanning functions are unused by every current binary.
--
-- This is a project migration because Centrum 0004 recreates the legacy
-- functions and grants after the platform chain runs. Cleanup must therefore
-- be the last word, not an earlier platform migration that 0004 would undo.
--
-- Contract step: destructive by design. Apply only after the stability window.

DROP FUNCTION IF EXISTS private.login_user(text, text);
DROP FUNCTION IF EXISTS private.create_session(uuid, text, timestamptz);
DROP FUNCTION IF EXISTS private."session_user"(text);

DO $$
BEGIN
  IF to_regnamespace('project_centrum') IS NOT NULL THEN
    -- auth_definer must not retain any access to project data. The provider
    -- reads sports_users as app_runtime inside the tenant transaction, where
    -- RLS applies.
    EXECUTE 'REVOKE ALL ON ALL TABLES IN SCHEMA project_centrum FROM auth_definer';
    EXECUTE 'REVOKE ALL ON ALL SEQUENCES IN SCHEMA project_centrum FROM auth_definer';
    EXECUTE 'REVOKE USAGE ON SCHEMA project_centrum FROM auth_definer';
  END IF;
END $$;

-- staff_session_create still verifies that the tenant is active. Grant only
-- the columns that check needs; db_schema was for the retired schema scanner.
REVOKE SELECT (db_schema) ON public.clients FROM auth_definer;
REVOKE SELECT ON public.clients FROM auth_definer;
GRANT SELECT (tenant_id, status) ON public.clients TO auth_definer;

COMMENT ON FUNCTION private.staff_session_create(text, uuid, text, timestamptz) IS
  'Writes a platform staff session for an already-authenticated (tenant, user) pair. The only clients columns it reads are tenant_id and status.';
