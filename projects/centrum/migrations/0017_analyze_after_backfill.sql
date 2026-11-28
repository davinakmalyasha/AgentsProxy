-- 0017_analyze_after_backfill.sql — refresh planner stats, no schema change.
--
-- Companion to platform 098 (public tables). Runs with search_path set to
-- the project schema by the migration runner, so unqualified names land
-- here. EXPLAIN 2026-11-28: facilities list Index Scan on
-- sports_facilities_live_sort_idx, pricing rule Index Scan Backward on
-- (tenant_id, valid_from), per-facility bookings Index Scan (exclusion
-- GiST), blocks Index Scan on (tenant_id, block_date). No new index.
ANALYZE sports_bookings;
ANALYZE sports_facilities;
ANALYZE sports_pricing_rules;
ANALYZE sports_court_blocks;
