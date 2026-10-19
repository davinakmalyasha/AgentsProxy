# Deferred — refinement, security, performance (NOT now)

Deliberately postponed during the Resilience First track. Each item has a
**trigger**: the event that promotes it to active work. Nothing here is
forgotten; nothing here blocks current usability.

Rules: nothing lands from this list without its own failing-first test and
a backward-compat note (frozen UI strings in `actions.ts` / `kantin/actions.ts`
+ `ProxyError` / `friendlyError` matching must keep passing).

---

## Refinement (code consistency, wider parity)

- **Status/method vocabulary**: `cash/qris`, booking states, role names
  re-declared per file; `NOT IN ('cancelled','no_show')` in 5 queries (a new
  state silently counts as revenue).
  Trigger: adding any new status/method, or tenant #2 with different states.
- **Pagination unification**: 5 schemes (cursor 20/100, ParseLimit 200/2000,
  hand-rolled 500/2000, silent clamp). All lists capped now; unify the helper later.
  Trigger: any new list endpoint, or a `truncated:true` complaint from UI.
- **Validation consistency**: patch empty-string acceptance, unvalidated
  thresholds/qty, per-table status enums.
  Trigger: a validation bug report, or new-entity work.
- **N+1 queries**: kantin sale loop (N×4 statements), correlated movement SUM,
  availability 3-sequential-queries + unbounded cache map.
  Trigger: measured p95 pain on those endpoints.
  DONE 2026-10 (perf pass): set-based sale/void (unnest), window-function
  stock_after, single-query availability, paginated customer visits,
  RuleAndPriceFor single lookup, batched reminder writes. The availability
  in-process map is bounded since the same pass (`maxAvailCacheEntries`
  + 3 s TTL, tenants/centrum-badminton/backend/availability/feature.go);
  the old "unbounded" note here was stale (corrected 2026-10-19).
- **Contracts on 2/14 features**: bookings + pricing gated; gate the rest or
  document exemptions.
  Trigger: first v2 schema change on an ungated feature.
  PARTIAL 2026-10: contract_gate_test locks the gated/exempt set — a new
  write path cannot slip in ungated without declaring itself. Full
  gating of the other 11 still open (needs per-feature verification).
- **Phone normalization**: DONE 2026-10-19 (065). `private.normalize_phone`
  canonicalizes to `+62…` for Indonesian shapes (foreign `+` numbers keep
  their code); SECURITY DEFINER BEFORE triggers on `sports_customers.phone`
  and `sports_bookings.customer_phone` cover every writer, and the existing
  UNIQUE (tenant_id, phone) now catches mixed-format duplicates. Backfill
  normalized existing rows with a collision guard. Pinned by
  TestNormalizePhone (function corpus + trigger storage behavior).
- **Cancel-note on HTTP**: currently optional; tool requires when paid.
  Trigger: owner asks for enforced money notes in the app flow.

## Security (tightening, later by design)

- **RBAC tightening**: `RequireStaff` (owner|pegawai, no client); expenses/payments
  reads to owner; `payments:patch` to owner. CHANGES who can do what —
  needs frontend coordination + owner sign-off, never silent.
  Trigger: second staff role, external API consumers, or any auth incident.
- **Notify driver PUBLIC-execute**: RESOLVED 2026-10-17 — already closed
  by 032's blanket REVOKE on the delivery surface (reminder drivers,
  notify trio, both probes, reap). The old "left untouched" note here
  was stale; a fresh migration chain and prod both show no PUBLIC
  entries (pg_proc ACLs verified). Pinned by
  TestNotifyDrivers_PublicExecuteClosed: app_runtime denied on the five
  notify fns/probes, delivery_caller allowed, app_runtime keeps its
  deliberate reap grant.
- **Fresh-DB RLS gap on clients/usage**: RLS on those two tables was
  enabled out-of-band on production (033), never by migration — so a
  database built purely from migrations has NO RLS there and the
  `app_runtime` table grants govern alone. Adding `ENABLE ROW LEVEL
  SECURITY` to a migration breaks the `app` TEST role (it has table
  grants but no policies, and a test-only role cannot be named in a
  production migration). Fixing properly needs test-role rework first.
  Trigger: new-environment provisioning, or the test-role cleanup.
- **GDPR export/erase**: customer delete orphans PII copies in bookings/payments.
  Trigger: customer request, second tenant, or regulatory need.
- **Admin-key rotation**: key-id + overlap, no flag day.
  Trigger: staff turnover or key exposure suspicion.
- **Session refresh**: 30-day non-rotating tokens.
  Trigger: any session-theft concern.
- **CORS allowlist**: `*` default lets any site call public endpoints (quota burn).
  Trigger: observed quota abuse.
- **Secret redaction gaps**: Sentry `scrubPII` covers password/secret only.
  Trigger: log audit.
  DONE 2026-10 (perf pass): sensitive list expanded (session/client tokens,
  cookies, api_key, dsn, database_url, token) + shared with the slog
  redactor via sentry.IsSensitive so the two cannot drift.

## Performance (measured work only — no premature optimization)

- **Missing indexes**: bookings customer-phone/name joins, users email expression
  index, inventory/member names, facilities sort_order, notify `(vt,id)` ordering,
  availability `(tenant,date,status)`.
  Trigger: `EXPLAIN ANALYZE` pain on those paths.
  DONE 2026-10 (migration 053/054, validated on Postgres 18): all of the
  above landed as partial/composite indexes, plus events_session_time_idx
  (was a seq scan per agent turn, now index-only), tenant_records list
  indexes, money WIB-day partials, and 12 write-amplifying duplicates
  dropped. Re-verify with EXPLAIN after real traffic (blocks in §11).
- **Availability cache decision**: RESOLVED 2026-10-19 — the in-process
  map is bounded (`maxAvailCacheEntries`, 3 s TTL, availability/feature.go);
  the stale "unbounded" note was wrong. The trigger below still stands for
  the per-instance nature.
  Trigger: multi-instance divergence or memory growth.
- **Quota-Redis role**: currently hint-or-deny hybrid; revisit if Redis load grows.
  Trigger: Redis cost/latency.
- **Prefix-cache accounting**: unmeasured ~30x LLM cost lever.
  Trigger: LLM bill review.
- **Retention/janitor**: usage/bookings/payments grow forever; backup unscheduled.
  Trigger: table-size alert, or the first restore drill.
  DONE 2026-10 (perf pass) for pruning: ops/retention.sql (pg_cron
  retention_tick for usage/events/notify_jobs/sessions/report backstop,
  batched deletes) + Go janitor/sweeper gated on BACKGROUND_TICKERS +
  retention indexes + BRIN. R18 DONE 2026-10-18: .github/workflows/
  backup.yml runs ops/backup.sh nightly with a backup_reader credential
  (062: LOGIN BYPASSRLS + pg_read_all_data; GitHub secret
  BACKUP_DATABASE_URL) and restores the dump into a postgres:17 smoke
  job. Platform layer: WAL-G daily backups on, pitr_enabled=false — the
  dump is the independent, off-platform copy. Retention tick installed
  on prod 2026-10-17 (retention-tick active); smoke.yml now probes the
  frontend too.
- **Pool tuning beyond admin**: runtime pool Min/Max per deploy shape.
  Trigger: another EMAXCONNSESSION-class incident.
- **ADK session-table RLS**: `sessions/events/app_states/user_states`
  are `USING (true)`; isolation rests entirely on every Go query
  filtering `app_name` (= tenant slug). Decision 2026-10: app-layer
  filtering verified complete on all 13 query sites (see the TENANT
  ISOLATION CONTRACT in `pgx_sessions.go`); DB-enforced policy needs
  tx-scoped `set_config` on the read paths (+3 RTs/turn on the hottest
  path) — do it only with integration coverage, not blind.
  ENFORCED 2026-10-10 by `TestSessionStore_TenantIsolation`
  (`internal/adkrunner/session_isolation_test.go`): cross-app Get/List/
  Delete leak checks plus fail-closed empty-app_name at all four entry
  points (Create/Get/List/Delete). The test, not the policy, is the
  enforcement until the GUC tradeoff is re-measured.
  Trigger: any raw-SQL-via-app_runtime path, or a second reviewer.
- **Rate-limiter consolidation**: 3 Lua round trips per request
  (global + scope + client). Merging needs middleware-chain
  restructuring around pre/post-auth identity — bypass risk outweighs
  ~4ms. If p99 says otherwise, the fix is moving Upstash to
  ap-southeast-1 (RUNBOOK-vercel), not code.
  Trigger: measured limiter latency on the request path.
- **Reminder delivery repair**: `reminder-tick` must stay unscheduled until
  a delivery target lands (was firing into the deleted `/deliver-reminders`;
  per 042 header, not the old keep-it-scheduled note — corrected 2026-10-11:
  a prod probe found no pg_cron at all, so there was never anything burning
  on prod; nothing to unschedule, just do not install it).
  Re-schedule via `ops/schedule-delivery.sql` (reminder line currently
  commented out) when the delivery target lands. The 3 orphaned
  driver SQL functions were dropped in 055; a repair re-adds what it
  needs (see 031/042/048 for the bodies).
  DONE 2026-10-18 (PWA Web Push, live-verified end to end):
  057 re-added the quarantine-aware drivers + push_subscriptions +
  recipient_ref; 059 moved the Edge driver to RPC-only DB access
  (delivery_caller holds no table grants) with release-without-burning
  parity; 060 fixed postgres.js millisecond truncation that made
  release/complete/fail never match the claim; the Edge encoder's
  missing RFC 8188 second HKDF extract was found only by a real-device
  test (FCM accepts undecryptable bodies) and is now pinned by
  crypto_test.ts; the frontend subscribes through a same-origin shim
  (the httpOnly session cookie made the old client-side read fail with
  "no session"). E2E proof: real Chrome subscription → FCM → displayed
  notification, plus the negative ladder (bogus endpoint → dead at 5,
  DNS error logged). VAPID keys + delivery_caller password rotated.

## Environments (deferred tiers — decision 2026-10-11)

- **Cloud staging + per-PR preview DBs**: deferred. Branching needs Pro
  (~$10/mo per persistent branch) and a second project adds daily
  connection-string juggling for zero benefit at current scale. Standing
  replacement, in order: (1) Laragon Postgres for dev (unchanged);
  (2) scratch-PG validation before every migration lands (full 001→latest
  chain + seeded edge cases + old-vs-new output diff, the 056 drill);
  (3) `main` branch protection + release-PR habit for prod.
  Triggers, any one reopens this: real traffic volume (EXPLAIN-after-volume
  needs a prod-like planner), a second collaborator, or revenue justifying
  Pro — then: separate staging project first, per-PR branches second, PITR
  with Pro.

## Done (moved here from planning — landed)

- Resilience First R1–R4 (members ledger scope, users scan, tombstone deletes,
  list limits, honest 4xx, tool parity, quarantine, contracts pilot).
- Admin pool sizing (Min 0 / Max 3 / 1m idle) after the 5432 exhaustion.
