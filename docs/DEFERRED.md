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
  in-process map is still unbounded per instance (see below).
- **Contracts on 2/14 features**: bookings + pricing gated; gate the rest or
  document exemptions.
  Trigger: first v2 schema change on an ungated feature.
  PARTIAL 2026-10: contract_gate_test locks the gated/exempt set — a new
  write path cannot slip in ungated without declaring itself. Full
  gating of the other 11 still open (needs per-feature verification).
- **Phone normalization**: `0812` vs `62812` vs `+62` split customer identity.
  Trigger: duplicate-customer complaint, or tenant #2.
- **Cancel-note on HTTP**: currently optional; tool requires when paid.
  Trigger: owner asks for enforced money notes in the app flow.

## Security (tightening, later by design)

- **RBAC tightening**: `RequireStaff` (owner|pegawai, no client); expenses/payments
  reads to owner; `payments:patch` to owner. CHANGES who can do what —
  needs frontend coordination + owner sign-off, never silent.
  Trigger: second staff role, external API consumers, or any auth incident.
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
- **Availability cache decision**: in-process map (unbounded, per-instance).
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
  retention indexes + BRIN. Backup scheduling (R18) still open.
- **Pool tuning beyond admin**: runtime pool Min/Max per deploy shape.
  Trigger: another EMAXCONNSESSION-class incident.

## Done (moved here from planning — landed)

- Resilience First R1–R4 (members ledger scope, users scan, tombstone deletes,
  list limits, honest 4xx, tool parity, quarantine, contracts pilot).
- Admin pool sizing (Min 0 / Max 3 / 1m idle) after the 5432 exhaustion.
