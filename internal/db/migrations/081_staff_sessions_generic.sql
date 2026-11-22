-- 081_staff_sessions_generic.sql — tenant-aware session primitives.
--
-- The old create_session only received a user id, so it had to scan every
-- active project to discover which schema owned the user. That is O(projects)
-- on every login and it kept the platform function coupled to project user
-- tables.
--
-- The project provider now authenticates the user first and passes the tenant
-- explicitly. create_session becomes a directory write: it validates that the
-- tenant is active and stores the session. It never reads a project schema.
--
-- The old 3-argument create_session and session_user stay until the contract
-- migration so a previous binary can serve through the deploy window. The new
-- binary calls staff_session_create / staff_session_resolve.
--
-- Additive: new functions only. delete_session and touch_session already only
-- touch public.staff_sessions and keep their signatures.

CREATE OR REPLACE FUNCTION private.staff_session_create(
  p_tenant     text,
  p_user_id    uuid,
  p_token_hash text,
  p_expires_at timestamptz
)
RETURNS uuid
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $function$
DECLARE
  v_id uuid;
BEGIN
  -- A paused or deleted tenant must not mint a session. This is the only
  -- lookup the platform function performs; the project provider has already
  -- verified that the user belongs to this tenant.
  IF NOT EXISTS (
    SELECT 1 FROM public.clients
     WHERE tenant_id = p_tenant AND status = 'active'
  ) THEN
    RETURN NULL;
  END IF;

  INSERT INTO public.staff_sessions (user_id, token_hash, expires_at, tenant_id)
  VALUES (p_user_id, p_token_hash, p_expires_at, p_tenant)
  ON CONFLICT (token_hash) DO UPDATE
    SET expires_at   = EXCLUDED.expires_at,
        tenant_id    = EXCLUDED.tenant_id,
        last_seen_at = pg_catalog.now()
  RETURNING id INTO v_id;
  RETURN v_id;
END;
$function$;

ALTER FUNCTION private.staff_session_create(text, uuid, text, timestamptz)
  OWNER TO auth_definer;
REVOKE ALL ON FUNCTION private.staff_session_create(text, uuid, text, timestamptz)
  FROM PUBLIC;
GRANT EXECUTE ON FUNCTION private.staff_session_create(text, uuid, text, timestamptz)
  TO app_runtime;

CREATE OR REPLACE FUNCTION private.staff_session_resolve(p_token_hash text)
RETURNS TABLE(user_id uuid, tenant_id text)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = ''
AS $function$
  SELECT s.user_id, s.tenant_id
    FROM public.staff_sessions s
   WHERE s.token_hash = p_token_hash
     AND s.expires_at > pg_catalog.now()
   LIMIT 1
$function$;

ALTER FUNCTION private.staff_session_resolve(text) OWNER TO auth_definer;
REVOKE ALL ON FUNCTION private.staff_session_resolve(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION private.staff_session_resolve(text) TO app_runtime;

COMMENT ON FUNCTION private.staff_session_create(text, uuid, text, timestamptz) IS
  'Writes a platform staff session for an already-authenticated (tenant, user) pair. Never reads a project schema.';
COMMENT ON FUNCTION private.staff_session_resolve(text) IS
  'Resolves a staff session token to (user_id, tenant_id). The project provider loads the live profile.';
