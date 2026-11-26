-- 096_drop_unused_indexes.sql — drop indexes no code path can use.
--
-- Each drop was verified against every query in the codebase, not just the
-- idle-prod "unused" advisory (an empty database never scans anything):
--
--   * tenant_records_slug_idx — no query filters by slug in either the
--     shared records handlers or any feature adapter.
--   * tenant_records_payload_gin_idx — no jsonb operators (@>, ?, etc.)
--     anywhere; the free-text search uses payload::text ILIKE, which a GIN
--     index cannot serve (that would need pg_trgm, deliberately not added).
--   * tenant_records_booking_date_idx — monolith-era expression
--     ((payload->>'date')) WHERE kind='booking'; the kind-scoped list is
--     served by tenant_records_kind_list_idx and nothing filters on the
--     payload date expression.
--   * agent_reminders_due_idx (status, remind_at) — the due-claim queries
--     (Go scheduler, claim RPCs) filter status='pending' and are served by
--     the smaller partial agent_reminders_claim_idx; the reaper and the
--     retention sweeper use claimed_at / created_at indexes instead.
--   * notify_jobs_tenant_idx — the notify worker is cross-tenant by design
--     (definer claim functions); no query filters notify_jobs by tenant.
--   * staff_sessions_tenant_idx — every session operation resolves by
--     token_hash (unique) or sweeps by expires_at; nothing lists sessions
--     by tenant.
--   * admin_audit_log_{occurred,actor,action}_idx — append-only audit with
--     no read path in code; sequential scans are fine at this volume, and
--     the indexes can return when a reader ships.
--   * report_objects_tenant_idx — downloads resolve by token_hash (unique),
--     the sweeper by expires_at; nothing filters report_objects by tenant.
--
-- DROP IF EXISTS only. Tables, policies, and grants are untouched.
-- Write amplification drops with every removed index.

DROP INDEX IF EXISTS public.tenant_records_slug_idx;
DROP INDEX IF EXISTS public.tenant_records_payload_gin_idx;
DROP INDEX IF EXISTS public.tenant_records_booking_date_idx;
DROP INDEX IF EXISTS public.agent_reminders_due_idx;
DROP INDEX IF EXISTS public.notify_jobs_tenant_idx;
DROP INDEX IF EXISTS public.staff_sessions_tenant_idx;
DROP INDEX IF EXISTS public.admin_audit_log_occurred_idx;
DROP INDEX IF EXISTS public.admin_audit_log_actor_idx;
DROP INDEX IF EXISTS public.admin_audit_log_action_idx;
DROP INDEX IF EXISTS public.report_objects_tenant_idx;
