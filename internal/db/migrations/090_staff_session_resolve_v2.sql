-- 090_staff_session_resolve_v2.sql — one-round-trip staff identity.
--
-- The old path was:
--   staff_session_resolve(token)           1 query
--   clients lookup inside the provider     1 query + transaction framing
--   sports_users profile                   1 query
--   clients entitlement                    1 query
--   touch_session(token)                   1 query
-- plus BEGIN/set_config/COMMIT. The same data is already in the session
-- directory and its owning clients row, so resolve it once.
--
-- v2:
--   * resolves the token and its tenant's live routing/entitlement fields;
--   * touches last_seen_at at most once a minute inside the same call;
--   * returns status so the caller still rejects a paused tenant;
--   * never returns a password hash or any project table row.
--
-- The old function stays for a rollback window. Drop it in a contract
-- migration after the code that calls v2 has been live through one release.

CREATE OR REPLACE FUNCTION private.staff_session_resolve_v2(p_token_hash text)
RETURNS TABLE(
  user_id   uuid,
  tenant_id text,
  client_id uuid,
  db_schema text,
  status    text,
  vertical  text,
  features  text[]
)
LANGUAGE plpgsql
VOLATILE
SECURITY DEFINER
SET search_path = ''
AS $function$
BEGIN
  -- A busy dashboard fires many requests; an unconditional UPDATE would
  -- take the session row lock on every one. The old Go path paid a separate
  -- round trip to do exactly this, so keep the throttle but move it into the
  -- resolve statement.
  UPDATE public.staff_sessions s
     SET last_seen_at = pg_catalog.now()
   WHERE s.token_hash = p_token_hash
     AND s.expires_at > pg_catalog.now()
     AND s.last_seen_at < pg_catalog.now() - interval '1 minute';

  RETURN QUERY
  SELECT s.user_id,
         s.tenant_id,
         c.id,
         c.db_schema,
         c.status,
         c.vertical,
         COALESCE(c.features, '{}'::text[])
    FROM public.staff_sessions s
    JOIN public.clients c ON c.tenant_id = s.tenant_id
   WHERE s.token_hash = p_token_hash
     AND s.expires_at > pg_catalog.now()
   LIMIT 1;
END;
$function$;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'auth_definer') THEN
    -- SELECT only the routing/entitlement columns. The session directory
    -- does not need the token hash, quota, name, or any other client field.
    GRANT SELECT (id, db_schema, status, vertical, features)
      ON public.clients TO auth_definer;
  END IF;
END $$;

ALTER FUNCTION private.staff_session_resolve_v2(text) OWNER TO auth_definer;
REVOKE ALL ON FUNCTION private.staff_session_resolve_v2(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION private.staff_session_resolve_v2(text) TO app_runtime;

COMMENT ON FUNCTION private.staff_session_resolve_v2(text) IS
  'Resolve a staff session token to its user and live tenant routing/entitlement in one round trip; touch last_seen_at at most once a minute.';
