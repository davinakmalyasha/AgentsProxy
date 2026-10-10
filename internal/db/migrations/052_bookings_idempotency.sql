-- 052_bookings_idempotency.sql — R3: agent + UI bookings share the
-- idempotency convention (optional phase). A retried booking with the same
-- key returns the original row instead of 409-confusing the caller.
-- Additive only (GOVERNANCE §3): nullable column + partial unique index.
-- The exclusion constraint still owns double-booking protection.

ALTER TABLE sports_bookings
  ADD COLUMN IF NOT EXISTS idempotency_key text;
CREATE UNIQUE INDEX IF NOT EXISTS sports_bookings_tenant_idem_uniq
  ON sports_bookings(tenant_id, idempotency_key)
  WHERE idempotency_key IS NOT NULL;
