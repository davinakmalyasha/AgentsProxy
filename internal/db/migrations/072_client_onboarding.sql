-- 072_client_onboarding.sql — self-service CLIENT registration + order ownership.
--
-- signup_requests holds a pending email-verified signup. The password is
-- already bcrypt-hashed; the 6-digit code is SHA-256 hashed. A user row is
-- only created after /auth/register/verify succeeds (single use, 10 min,
-- 5 attempts). Pending rows are cleaned opportunistically on new signups.
--
-- orders.user_id binds a web order to the signed-in client immediately, so
-- the portal shows it without waiting for an admin to link the lead. The
-- legacy users.lead_id path keeps working for staff-linked accounts.
-- Additive-only: new table, new nullable column, new index.
CREATE TABLE IF NOT EXISTS signup_requests (
    id            BIGSERIAL PRIMARY KEY,
    username      TEXT NOT NULL,
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    code_hash     TEXT NOT NULL,
    attempts      INTEGER NOT NULL DEFAULT 0,
    expires_at    TIMESTAMPTZ NOT NULL,
    consumed_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS signup_requests_expires_idx ON signup_requests (expires_at);

ALTER TABLE orders ADD COLUMN IF NOT EXISTS user_id BIGINT REFERENCES users (id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS orders_user_idx ON orders (user_id);

GRANT SELECT, INSERT, UPDATE, DELETE ON signup_requests TO app_runtime;
GRANT USAGE, SELECT ON signup_requests_id_seq TO app_runtime;
