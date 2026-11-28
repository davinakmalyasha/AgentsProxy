-- 098_analyze_after_backfill.sql — refresh planner stats, no schema change.
--
-- 088 backfilled project entitlements and 053/096 reshaped indexes; without
-- fresh stats the planner can cling to a stale row-count and pick a seq scan
-- on a table that now has the right index. ANALYZE is cheap, runs in-txn,
-- and touches no data or policy. Free-plan safe. Verified: EXPLAIN on
-- project_centrum (2026-11-28) shows facilities/pricing/bookings/blocks all
-- Index Scan with existing indexes; clients/usage seq scans are correct at
-- 7/1 rows and switch to clients_token_hash_key /
-- usage_client_created_include_idx at volume. No new index warranted.
ANALYZE public.clients;
ANALYZE public.usage;
ANALYZE public.tenant_records;
ANALYZE public.events;
ANALYZE public.notify_jobs;
ANALYZE public.agent_reminders;
ANALYZE project_centrum.sports_bookings;
ANALYZE project_centrum.sports_facilities;
ANALYZE project_centrum.sports_pricing_rules;
ANALYZE project_centrum.sports_court_blocks;
