-- 095_index_prefix_cleanup.sql — drop redundant prefix indexes, index the reap.
--
-- Verified prefix drops (each surviving index serves every query the
-- dropped one served; proven with EXPLAIN on a seeded database, not just
-- the idle-prod "unused" advisory):
--
--   * tenant_records_tenant_vertical_kind_idx (tenant_id, vertical, kind)
--     is a leading prefix of tenant_records_kind_list_idx
--     (tenant_id, vertical, kind, status, created_at DESC, id DESC).
--     The (tenant_id, vertical, kind) lookup resolves to an Index Only
--     Scan on kind_list_idx with an identical plan after the drop.
--
--   * report_jobs_working_claimed_idx is new: the report tick reaps
--     status='working' rows older than 10 minutes, and the only index was
--     on created_at WHERE pending — so every tick seq-scanned. The reap
--     now resolves to an Index Scan on (claimed_at) WHERE working.
--
-- Additive-safe: DROP IF EXISTS + CREATE IF NOT EXISTS. Write amplification
-- drops with the indexes; the new partial index only covers 'working' rows,
-- which are ~zero in steady state.

DROP INDEX IF EXISTS public.tenant_records_tenant_vertical_kind_idx;

CREATE INDEX IF NOT EXISTS report_jobs_working_claimed_idx
  ON public.report_jobs (claimed_at)
  WHERE status = 'working';
