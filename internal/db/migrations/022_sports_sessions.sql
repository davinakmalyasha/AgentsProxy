-- 022_sports_sessions.sql — Phase 7: staff login sessions.
-- DONE: Phase 7 (staff auth) / STANDARDS A, D, F
--
-- Session tokens are random 32-byte, sha256-hashed at rest (same
-- policy as client tokens). RLS is open — this is an internal lookup
-- table keyed by token_hash, never client-addressable (like
-- agent_bindings). Admin/superuser manages sessions.

CREATE TABLE IF NOT EXISTS sports_sessions (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id     uuid NOT NULL REFERENCES sports_users(id) ON DELETE CASCADE,
  token_hash  text NOT NULL UNIQUE,            -- sha256(raw token)
  expires_at  timestamptz NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  last_seen_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS sports_sessions_user_idx ON sports_sessions(user_id);
CREATE INDEX IF NOT EXISTS sports_sessions_token_idx ON sports_sessions(token_hash);

ALTER TABLE sports_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE sports_sessions FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS sports_sessions_open ON sports_sessions;
CREATE POLICY sports_sessions_open ON sports_sessions
  FOR ALL
  USING (true);

GRANT SELECT, INSERT, UPDATE, DELETE ON sports_sessions TO app_runtime;
