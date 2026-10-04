-- 046_ledger_tombstones.sql — Phase B2: hard DELETEs on ledgers become
-- tombstones. A deleted row is preserved (who, when) and hidden from
-- lists/reads/totals by default.
--
-- Additive only (GOVERNANCE §3): nullable columns, no backfill needed
-- (NULL = not deleted). Handlers set deleted_at/deleted_by instead of
-- DELETE; list/getOne/totals filter deleted_at IS NULL.
--
-- Tables: sports_payments, sports_expenses, sports_customers,
-- sports_facilities, sports_pricing_rules, sports_recurring_series.
-- (Kantin sales already soft-void via voided_at; bookings already
-- soft-cancel via status.)

ALTER TABLE sports_payments
  ADD COLUMN IF NOT EXISTS deleted_at timestamptz,
  ADD COLUMN IF NOT EXISTS deleted_by text NOT NULL DEFAULT '';
ALTER TABLE sports_expenses
  ADD COLUMN IF NOT EXISTS deleted_at timestamptz,
  ADD COLUMN IF NOT EXISTS deleted_by text NOT NULL DEFAULT '';
ALTER TABLE sports_customers
  ADD COLUMN IF NOT EXISTS deleted_at timestamptz,
  ADD COLUMN IF NOT EXISTS deleted_by text NOT NULL DEFAULT '';
ALTER TABLE sports_facilities
  ADD COLUMN IF NOT EXISTS deleted_at timestamptz,
  ADD COLUMN IF NOT EXISTS deleted_by text NOT NULL DEFAULT '';
ALTER TABLE sports_pricing_rules
  ADD COLUMN IF NOT EXISTS deleted_at timestamptz,
  ADD COLUMN IF NOT EXISTS deleted_by text NOT NULL DEFAULT '';
ALTER TABLE sports_recurring_series
  ADD COLUMN IF NOT EXISTS deleted_at timestamptz,
  ADD COLUMN IF NOT EXISTS deleted_by text NOT NULL DEFAULT '';
