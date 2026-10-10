-- 050_topup_ledger_member_scope.sql — R1a: the topup idempotency key must
-- be scoped to the member, not just the tenant. A key reused on a different
-- member must not return the wrong member's balance.
--
-- Additive-safe: drop + recreate one partial unique index (no data change),
-- plus FORCE RLS on the 045 tables (045 set ENABLE only; every other tenant
-- table uses FORCE so the table owner cannot bypass either).
-- Handlers change in the same release (single-tx dedup with member_id).

DROP INDEX IF EXISTS sports_member_topups_tenant_idem_uniq;
CREATE UNIQUE INDEX IF NOT EXISTS sports_member_topups_tenant_idem_member_uniq
  ON sports_member_topups(tenant_id, idempotency_key, member_id)
  WHERE idempotency_key IS NOT NULL;

ALTER TABLE sports_member_topups FORCE ROW LEVEL SECURITY;
ALTER TABLE sports_payments FORCE ROW LEVEL SECURITY;
ALTER TABLE sports_kantin_sales FORCE ROW LEVEL SECURITY;
