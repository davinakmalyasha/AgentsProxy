-- 021_sports_users.sql — Phase 7: owner/pegawai staff accounts.
-- DONE: Phase 7 (staff auth) / STANDARDS A, D, F
--
-- Unified identity: staff log in with email+password → bcrypt-hashed
-- password → session token (022). The tenant's client token (API /
-- Telegram agent) keeps full access for backward compatibility.

CREATE TABLE IF NOT EXISTS sports_users (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  email         text NOT NULL,
  password_hash text NOT NULL,                 -- bcrypt
  role          text NOT NULL DEFAULT 'pegawai' CHECK (role IN ('owner','pegawai')),
  full_name     text NOT NULL DEFAULT '',
  is_active     boolean NOT NULL DEFAULT true,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, email)
);

CREATE INDEX IF NOT EXISTS sports_users_tenant_idx ON sports_users(tenant_id);

-- Login needs to find a user by (tenant_id, email) with no tenant GUC
-- set, so a bare SELECT cannot be tenant-scoped. The old
-- `sports_users_read_all ... USING (true)` solved that by exposing every
-- tenant's bcrypt hashes to the runtime role. Instead, reads go through
-- `private.login_user` (below): a SECURITY DEFINER function that takes
-- the tenant as an argument and returns exactly the five columns login
-- needs. The table policy is now tenant-scoped like every other table.
ALTER TABLE sports_users ENABLE ROW LEVEL SECURITY;
ALTER TABLE sports_users FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS sports_users_read_all ON sports_users;
DROP POLICY IF EXISTS sports_users_read ON sports_users;
CREATE POLICY sports_users_read ON sports_users
  FOR SELECT
  USING (tenant_id = current_setting('app.tenant_id', true));

-- auth_definer is created here (not 032) because login_user needs an
-- owner. NOLOGIN BYPASSRLS, same audited shape as delivery_worker
-- (031): a capability holder, not a credential. BYPASSRLS because FORCE
-- RLS would otherwise filter the definer's own reads to zero rows.
-- Holds grants on exactly one table. 032's creation is IF NOT EXISTS,
-- so order is safe either way.
--
-- The private schema is born here (not 031) because login_user below
-- needs a home. 031's CREATE SCHEMA IF NOT EXISTS is idempotent, so a
-- later creation is a no-op.
CREATE SCHEMA IF NOT EXISTS private;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'auth_definer') THEN
    CREATE ROLE auth_definer NOLOGIN BYPASSRLS;
  END IF;
END $$;

GRANT USAGE ON SCHEMA private TO auth_definer;
GRANT SELECT ON sports_users TO auth_definer;

-- Ownership transfer (ALTER ... OWNER TO) requires two things the fresh
-- role lacks: postgres must be a MEMBER of auth_definer, and
-- auth_definer must hold CREATE on the schema its functions live in.
-- On a local superuser both are implicit; on Supabase (postgres is
-- CREATEROLE but not superuser) both must be granted explicitly.
-- Verified against Supabase before committing this pattern.
GRANT auth_definer TO postgres;
GRANT USAGE, CREATE ON SCHEMA private TO auth_definer;

-- Login lookup. Tenant comes from the caller as data, never from a GUC,
-- so there is no ambient-tenant confusion. Returns the hash because
-- bcrypt verification happens in Go; nothing else reads this function.
CREATE OR REPLACE FUNCTION private.login_user(p_tenant text, p_email text)
RETURNS TABLE(user_id uuid, password_hash text, role text, full_name text, is_active boolean)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = '' AS $$
  SELECT u.id, u.password_hash, u.role, u.full_name, u.is_active
    FROM public.sports_users u
   WHERE u.tenant_id = p_tenant AND u.email = p_email;
$$;
ALTER FUNCTION private.login_user(text, text) OWNER TO auth_definer;
REVOKE ALL ON FUNCTION private.login_user(text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION private.login_user(text, text) TO app_runtime;

-- Tenant-scoped writes: the INSERT/UPDATE/DELETE policies share one
-- name so a single statement can carry the full policy set.
DROP POLICY IF EXISTS sports_users_write ON sports_users;
CREATE POLICY sports_users_write ON sports_users
  FOR INSERT
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS sports_users_update ON sports_users;
CREATE POLICY sports_users_update ON sports_users
  FOR UPDATE
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS sports_users_delete ON sports_users;
CREATE POLICY sports_users_delete ON sports_users
  FOR DELETE
  USING (tenant_id = current_setting('app.tenant_id', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON sports_users TO app_runtime;
