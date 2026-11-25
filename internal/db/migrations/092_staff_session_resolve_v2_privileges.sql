-- 092_staff_session_resolve_v2_privileges.sql — privilege repair for v2.
--
-- The fresh-database CI chain (PostgreSQL 17) can execute the original
-- column-granted clients read in one environment but denied it as
-- auth_definer in another. The function body is unchanged and still selects
-- only routing/entitlement columns; this migration makes the capability
-- explicit and idempotent:
--
--   * pin the function owner and SECURITY DEFINER property;
--   * grant schema USAGE (078 does this, but the repair must be standalone);
--   * grant table SELECT on clients to the definer role.
--
-- The table-level grant is intentionally narrow in effect: auth_definer is
-- NOLOGIN, and this function only ever returns id/db_schema/status/vertical/
-- features. app_runtime still has no access to the table directly through
-- this role.
--
-- Additive/idempotent: grants and ALTER FUNCTION only.

ALTER FUNCTION private.staff_session_resolve_v2(text) OWNER TO auth_definer;
ALTER FUNCTION private.staff_session_resolve_v2(text) SECURITY DEFINER;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'auth_definer') THEN
    GRANT USAGE ON SCHEMA public TO auth_definer;
    GRANT SELECT ON public.clients TO auth_definer;
  END IF;
END $$;

REVOKE ALL ON FUNCTION private.staff_session_resolve_v2(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION private.staff_session_resolve_v2(text) TO app_runtime;
