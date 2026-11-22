-- 078_platform_staff_sessions.sql — staff sessions are platform auth state.
--
-- A staff session token is presented without a tenant parameter. Resolving
-- it from a per-project sports_sessions table would require scanning every
-- project schema, and it would keep the auth functions hardcoded to
-- project_centrum (which breaks cloned schemas and any future project).
--
-- The session directory moves to public.staff_sessions. The schema that owns
-- the user is still resolved from clients.db_schema (now unique), and the
-- auth functions read that project's sports_users through a definer.
--
-- The old project_centrum.sports_sessions table is copied, not dropped here.
-- Centrum's 0004 switches the functions; 0005 drops the legacy table after
-- the contract window.
--
-- Additive: new table, indexes, grants; existing rows copied.

CREATE TABLE IF NOT EXISTS public.staff_sessions (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  -- No FK: user_id points into a project schema, which is selected at
  -- runtime from clients.db_schema. The definer verifies the user on every
  -- lookup, so a deleted user invalidates their sessions immediately.
  user_id      uuid NOT NULL,
  token_hash   text NOT NULL,
  expires_at   timestamptz NOT NULL,
  tenant_id    text NOT NULL REFERENCES public.clients(tenant_id) ON DELETE CASCADE,
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_seen_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT staff_sessions_token_hash_key UNIQUE (token_hash)
);

CREATE INDEX IF NOT EXISTS staff_sessions_tenant_idx
  ON public.staff_sessions (tenant_id);
CREATE INDEX IF NOT EXISTS staff_sessions_expires_idx
  ON public.staff_sessions (expires_at);

ALTER TABLE public.staff_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.staff_sessions FORCE ROW LEVEL SECURITY;

REVOKE ALL ON public.staff_sessions FROM PUBLIC;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'auth_definer') THEN
    GRANT USAGE ON SCHEMA public TO auth_definer;
    GRANT SELECT, INSERT, UPDATE, DELETE ON public.staff_sessions TO auth_definer;
    -- The auth functions resolve the user's schema from clients. Column
    -- grants keep the rest of the client row out of the capability.
    GRANT SELECT (tenant_id, db_schema, status) ON public.clients TO auth_definer;
  END IF;
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'anon') THEN
    REVOKE ALL ON public.staff_sessions FROM anon;
  END IF;
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'authenticated') THEN
    REVOKE ALL ON public.staff_sessions FROM authenticated;
  END IF;
END $$;

-- Preserve live sessions across the cutover. The old table is optional on a
-- fresh database; on production it exists and is the source of truth until
-- Centrum 0004 replaces the functions.
DO $$
BEGIN
  IF to_regclass('project_centrum.sports_sessions') IS NOT NULL THEN
    INSERT INTO public.staff_sessions
      (id, user_id, token_hash, expires_at, tenant_id, created_at, last_seen_at)
    SELECT id, user_id, token_hash, expires_at, tenant_id, created_at, last_seen_at
      FROM project_centrum.sports_sessions
     WHERE expires_at > now() - interval '7 days'
    ON CONFLICT (token_hash) DO NOTHING;
  END IF;
END $$;

COMMENT ON TABLE public.staff_sessions IS
  'Platform staff-session directory. It resolves the token to a tenant; the tenant row resolves the project schema that owns the user.';
