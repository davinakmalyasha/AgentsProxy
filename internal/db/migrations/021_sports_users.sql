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

-- The auth lookup (login) must find a user by (tenant_id, email)
-- WITHOUT a tenant GUC, so SELECT is open to app_runtime. Writes are
-- tenant-scoped via RLS like every other table.
ALTER TABLE sports_users ENABLE ROW LEVEL SECURITY;
ALTER TABLE sports_users FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS sports_users_read_all ON sports_users;
CREATE POLICY sports_users_read_all ON sports_users
  FOR SELECT
  USING (true);

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
