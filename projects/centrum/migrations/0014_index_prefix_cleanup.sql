-- 0014_index_prefix_cleanup.sql — drop redundant prefix indexes.
--
-- Same prefix proof as platform migration 095, for the project schema.
-- This file runs with search_path set to the project schema.
--
--   * tenant_records_tenant_vertical_kind_idx (tenant_id, vertical, kind)
--     is a leading prefix of tenant_records_kind_list_idx. Verified: the
--     (tenant_id, vertical, kind) lookup keeps an identical Index Only
--     Scan plan on kind_list_idx after the drop.
--
--   * sports_bookings_tenant_date_idx (tenant_id, date) is served by
--     sports_bookings_tenant_date_time_idx
--     (tenant_id, date DESC, start_time DESC, INCLUDE ...). A btree serves
--     an equality-prefixed leading column in either scan direction, and on
--     a seeded database the planner already preferred the composite index
--     for the (tenant_id, date) query before the drop — same plan after.
--
-- DROP IF EXISTS only; no table or data change.

DROP INDEX IF EXISTS tenant_records_tenant_vertical_kind_idx;
DROP INDEX IF EXISTS sports_bookings_tenant_date_idx;
