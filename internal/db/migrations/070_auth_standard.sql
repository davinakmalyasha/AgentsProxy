-- 070_auth_standard.sql — Google login + superadmin email OTP.
--
-- users.google_sub links a Google account (verified ID token subject).
-- UNIQUE so one Google account maps to exactly one user; NULL for
-- password-only accounts. Role never comes from Google: JIT provisioning
-- creates CLIENT rows only, staff linking never elevates.
--
-- login_otps holds SHA-256 hashes (never plaintext) of 6-digit codes for
-- the superadmin second step. Short expiry, bounded attempts, single use.
-- Additive-only: new column + new table, no existing rows touched.

ALTER TABLE users ADD COLUMN IF NOT EXISTS google_sub TEXT UNIQUE;

CREATE TABLE IF NOT EXISTS login_otps (
  id          BIGSERIAL PRIMARY KEY,
  user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  code_hash   TEXT NOT NULL,
  expires_at  TIMESTAMPTZ NOT NULL,
  attempts    INTEGER NOT NULL DEFAULT 0,
  used_at     TIMESTAMPTZ,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS login_otps_user_idx ON login_otps (user_id);

GRANT SELECT, INSERT, UPDATE, DELETE ON login_otps TO app_runtime;
GRANT USAGE, SELECT ON login_otps_id_seq TO app_runtime;
