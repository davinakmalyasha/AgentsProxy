-- 0015_drop_unused_indexes.sql — drop indexes no code path can use.
--
-- Same rule as platform migration 096: every drop verified against the
-- queries that actually run. This file runs with search_path set to the
-- project schema.
--
--   * tenant_records_slug_idx / tenant_records_payload_gin_idx — same proof
--     as the platform table (no slug predicates, no jsonb operators).
--   * sports_customers_tenant_idx (tenant_id, name) — every customer read
--     filters deleted_at IS NULL (list, getOne, phone lookups use the phone
--     unique key), so the partial live_name index serves all of them.
--   * sports_expenses_tenant_spent_idx (tenant_id, spent_at) — every expense
--     read filters deleted_at IS NULL and sorts spent_at DESC, served by the
--     partial live_spent index.
--   * sports_members_tenant_idx — the list filters deleted_at IS NULL and
--     sorts by name (live_name serves); every other read is by id (pkey) or
--     by member_id (topup ledger index).
--   * sports_payments_booking_idx (booking_id) — every booking-scoped
--     payment read filters deleted_at IS NULL and is served index-only by
--     booking_live_idx (booking_id, amount); single-row reads use the pkey.
--   * sports_facilities_tenant_idx — every facility read filters
--     deleted_at IS NULL (live_sort serves); the delete guard counts via
--     tenant_facility_idx.
--   * sports_kantin_sales_live_idx (tenant, created_at WHERE voided) — the
--     ledger export deliberately includes voided rows (ORDER BY created_at
--     is served by tenant_created_idx) and the money queries bucket by WIB
--     day (live_wibday serves); nothing matches the partial created_at
--     shape.
--   * sports_kantin_sales_live_id_idx — partial UNIQUE adds no constraint
--     beyond the pkey, and no query filters id + voided_at together.
--
-- Deliberately kept (verified in use): tenant_paid_idx (payments ORDER BY
-- paid_at), both recorder pairs (sort vs WIB-filter shapes), the export's
-- tenant_created_idx (includes voided rows), series/customer/facility
-- lookup indexes, and every table's sole tenant access path.
--
-- DROP IF EXISTS only. Tables, policies, and grants are untouched.

DROP INDEX IF EXISTS tenant_records_slug_idx;
DROP INDEX IF EXISTS tenant_records_payload_gin_idx;
DROP INDEX IF EXISTS sports_customers_tenant_idx;
DROP INDEX IF EXISTS sports_expenses_tenant_spent_idx;
DROP INDEX IF EXISTS sports_members_tenant_idx;
DROP INDEX IF EXISTS sports_payments_booking_idx;
DROP INDEX IF EXISTS sports_facilities_tenant_idx;
DROP INDEX IF EXISTS sports_kantin_sales_live_idx;
DROP INDEX IF EXISTS sports_kantin_sales_live_id_idx;
