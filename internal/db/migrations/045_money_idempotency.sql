-- 045_money_idempotency.sql — Phase B1: idempotency keys on money writes.
-- Double-click / retried POST must never create a second charge.
--
-- Additive only (GOVERNANCE §3): nullable columns, partial unique indexes,
-- one new ledger table. Handlers treat a missing key exactly as today
-- (optional-first phase); enforcement later.

-- 1. Payments: key on the row itself (one row per charge).
ALTER TABLE sports_payments
  ADD COLUMN IF NOT EXISTS idempotency_key text;
CREATE UNIQUE INDEX IF NOT EXISTS sports_payments_tenant_idem_uniq
  ON sports_payments(tenant_id, idempotency_key)
  WHERE idempotency_key IS NOT NULL;

-- 2. Kantin sales: key on the sale header (items hang off sale_id).
ALTER TABLE sports_kantin_sales
  ADD COLUMN IF NOT EXISTS idempotency_key text;
CREATE UNIQUE INDEX IF NOT EXISTS sports_kantin_sales_tenant_idem_uniq
  ON sports_kantin_sales(tenant_id, idempotency_key)
  WHERE idempotency_key IS NOT NULL;

-- 3. Member topups: topup is an UPDATE today, so the key needs a ledger.
-- Each topup inserts exactly one ledger row; the balance UPDATE runs in the
-- same transaction. A duplicate key returns the existing ledger row and the
-- current balance without touching money.
CREATE TABLE IF NOT EXISTS sports_member_topups (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  member_id       uuid NOT NULL REFERENCES sports_members(id) ON DELETE CASCADE,
  amount          numeric(12,2) NOT NULL CHECK (amount > 0),
  idempotency_key text,
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS sports_member_topups_tenant_idem_uniq
  ON sports_member_topups(tenant_id, idempotency_key)
  WHERE idempotency_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS sports_member_topups_member_idx
  ON sports_member_topups(member_id, created_at);

ALTER TABLE sports_member_topups ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS sports_member_topups_isolation ON sports_member_topups;
CREATE POLICY sports_member_topups_isolation ON sports_member_topups
  FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON sports_member_topups TO app_runtime;
