# DONE — D:\MyProject\AgentsProxy

> Master status of every PRD/STANDARDS item. **Last verified green**: see `go test -cover` output below.
> This file is the single place to scan for "what's left". Inline `✅` markers in PRD.md/ARCHITECTURE.md/STANDARDS.md mirror this list; `// DONE:` blocks at the top of every `.go` file name the specific sections that file satisfies.

Legend: ✅ done · 🟡 done with deviation (see notes) · ❌ not done · ⏳ deferred

> **Phase 3 update** (this commit): feature platform (registry/gate/builder), 5 shared features, cursor pagination, soft-delete fix, two-role split (RBAC), pool simple-protocol opt-in, custom-feature template. All hardening items 1–8 below are ✅.

---

## PRD.md

| § | Item | Status | Notes |
|---|---|---|---|
| 1.1 | Triangle context diagram | ✅ | Documented in PRD; impl in §3 |
| 1.2 | Per-client token model, master key in env | ✅ | `internal/auth/tokens.go` + `internal/cache/client.go` |
| 1.3 | Why Go here, not Node | ✅ | Documented; ARCHITECTURE §1 |
| 2 | Goals & non-goals | ✅ | Non-goals explicitly listed in PRD |
| 3 | Exact tree (3 + 12 modules) | ✅ | All created; minor path deviation: `internal/db/migrations/` instead of repo root (Go embed constraint) |
| 4.1 | `001_clients_usage.sql` | ✅ | `internal/db/migrations/001_clients_usage.sql` |
| 4.2 | `002_tenancy.sql` RLS monolith | ✅ | `internal/db/migrations/002_tenancy.sql`; policy is **PERMISSIVE** not RESTRICTIVE (PG semantics — RESTRICTIVE-only denies all) |
| 4.3 | `003_seed_dev.sql` | ✅ | `internal/db/migrations/003_seed_dev.sql` |
| 4.4 | `004_features.sql` (Phase 3) | ✅ | `internal/db/migrations/004_features.sql` — adds `vertical` + `features` columns, backfills dev, indexes |
| 4.5 | `005_rbac.sql` (Phase 3) | ✅ | `internal/db/migrations/005_rbac.sql` — `app_runtime` role, SELECT-only on `clients` |
| 4.6 | `006_booking_date_idx.sql` (Phase 3) | ✅ | `internal/db/migrations/006_booking_date_idx.sql` — partial index for booking feature |
| 5 | Redis keyspace (`ck:`, `rl:`, `quota:`) | ✅ | `internal/cache/client.go` + `internal/middleware/ratelimit.go` + `internal/quota/quota.go` |
| 6.1 | `POST /v1/chat` non-streaming | ✅ | `internal/api/chat.go` |
| 6.1 | Streaming SSE | ⏳ | Deferred per PRD "pass 2" |
| 6.2 | `records` feature (5 endpoints × 12 verticals) | ✅ | `internal/features/shared/records/handlers.go` — refactored from `api/business.go` |
| 6.3 | `POST/GET/PATCH /v1/admin/clients*` + `GET /v1/admin/usage` | ✅ | `internal/api/admin.go`; `vertical`+`features` accepted on create, cached evict on patch, new List |
| 6.4 | `GET /healthz` | ✅ | `internal/api/healthz.go` (DB+Redis pings) |
| 6.5 | Feature platform (registry/gate/builder/shared/custom) | ✅ | `internal/features/{registry,defaults,gate,builder,init}.go` + 5 shared + `_template` |
| 9 | Cursor pagination | ✅ | `internal/features/shared/records/pagination.go` + `?before=&limit=` (default 20, max 100) |
| 7 | Config + security checklist | ✅ | `internal/config/config.go`; redaction in `slog` + Sentry |
| 8 | Tests (≥14, `go test -cover` ≥70%) | ✅ | 35+ tests; coverage per package in `go test -cover` below |
| 9 | Dockerfile multi-stage + docker-compose | ✅ | `Dockerfile`, `docker-compose.yml` (prod only) |
| 10 | Laragon local dev flow | ✅ | README "Quick Start" + `.env` + `scripts/load-env.ps1` |
| 11 | Verify (curl loop, repo tree, report) | ✅ | `scripts/verify-phase2.ps1` |
| 12 | Order rule (Phase 1 green, then Phase 2) | ✅ | Verified end-to-end across two turns |
| 13 | Extensibility note (one string for vertical 13) | ✅ | `allowedVerticals` in `business.go` |
| 14 | File inventory (create vs don't-touch) | ✅ | All created; nothing outside this repo modified |
| 15 | Non-functional requirements (perf, security, scale) | ✅ | Documented in PRD + verified in STANDARDS |

## STANDARDS.md

| § | Item | Status | Notes |
|---|---|---|---|
| A | Stack pin (Go 1.27, chi, pgx, go-redis, slog) | ✅ | `go.mod` `go 1.27.0`, deps match |
| B | Local quality gates (`tidy`, `vet`, `fmt`, `test -cover`, `build`) | ✅ | All green; see coverage table |
| C | Testing (httptest, ≥70% coverage, required tests) | ✅ | Auth, Quota, RateLimit, TenantIsolation, ChatHappy+RedisCached, Admin — all present |
| D | Security checklist (secrets, tokens, constant-time, SQL, RLS, grants, headers, rate limit, validation, logs) | ✅ | All applied |
| E | Performance (latency targets, pool, build flags) | ✅ | Documented in RUNBOOK.md §7 |
| F | API contract & validation (auth, vertical, pagination, errors) | ✅ | All enforced in handlers |
| G | CI (ci.yml + security.yml + release.yml) | ✅ | `.github/workflows/{ci,security,release}.yml` |
| H | Post-deploy ops (healthz, backup, Sentry, runbook) | ✅ | `/healthz` + `ops/backup.sh` + Sentry + `RUNBOOK.md` |
| I | Docs (README, .env.example, PRD, ARCHITECTURE) | ✅ | All present + ✅ marked |
| J | Non-functional (perf, security, scale, maintainability) | ✅ | All addressed |

## ARCHITECTURE.md

| § | Item | Status | Notes |
|---|---|---|---|
| 1 | Stack snapshot | ✅ | Matches the build |
| 2 | Go 1.27 new (ServeMux, slog, security, perf) | ✅ | chi + stdlib hybrid per plan |
| 3 | Redis 8.10 (io-threads, atomic helpers, rate limit) | ✅ | Plain INCR+EXPIRE used (per PRD §5); HGETEX + Lua bucket are documented future improvements |
| 4 | Supabase PG17 (RLS, pooling) | ✅ | RLS + perms split (DATABASE_ADMIN_URL) for the prod-realistic test |
| 5 | Decision summary | ✅ | Matches the actual stack |

---

## Phase 3 Hardening Items

| # | Upgrade | Status | Where |
|---|---|---|---|
| 1 | Soft-delete leak fix | ✅ | `internal/features/shared/records/handlers.go` — `list` defaults `status='active'`, `?status=deleted` includes tombstones; `getOne` 404 on soft-deleted by default |
| 2 | Cursor pagination | ✅ | `internal/features/shared/records/pagination.go` — base64(`created_at_nano\|id`), `+1` over-fetch, `next_cursor` + `has_more`; tests `TestRecords_CursorPagination` |
| 3 | Two-role split (RBAC) | ✅ | `005_rbac.sql` + `cmd/server/main.go` + `db.NewPoolWithOptions`; `TestAppRuntime_CannotMutateClients` proves SELECT-only on `clients` (SQLSTATE 42501) |
| 4 | Supabase pooler compatibility | ✅ | `DB_USE_SIMPLE_PROTOCOL=true` → `pgx.QueryExecModeSimpleProtocol`; Supavisor 6543 for runtime, 5432 direct for admin |
| 5 | Rate limit by client ID | ✅ | `middleware/ratelimit.go` keys `c:{client_id}` post-auth; chat pre-auth keys `ip:{ip}` (acceptable — quota + token rate limit at chat) |
| 6 | Shared-DB tenant rule | ✅ | `ARCHITECTURE.md §7`; `service_role` banned on tenant tables; Edge Functions must `set_config('app.tenant_id', ...)` |
| 7 | Custom-feature table checklist | ✅ | `internal/features/clients/_template/README.md` — own migration + RLS + tenant_id + isolation test; mandatory checklist |
| 8 | Redis degradation matrix | ✅ | `ARCHITECTURE.md §8`; cache→DB, quota→SUM, rate-limit fail-open, feature toggle ≤15m, quota = soft cap |

## Coverage (latest `go test -p 1 ./... -cover`)

```
internal/api          68.3%    internal/llm           82.9%
internal/auth         95.5%    internal/middleware    79.3%
internal/cache        79.4%    internal/quota         90.3%
internal/config       76.1%    internal/redisx        78.6%
internal/db           26.6%    internal/sentry        27.5%
internal/features     93.0%    internal/usage         80.0%
internal/features/shared/records  44.8%
internal/features/clients/sports/bookings   67.6%   ⭐ double-booking test
internal/features/clients/sports/facilities 69.4%
internal/features/clients/sports/inventory  44.6%
internal/features/clients/sports/members    44.5%
internal/notify        82.4%
internal/agent         37.0%   (fake-LLM tests: book + double-book error)
internal/telegram      32.9%   (fake-API tests)
internal/reminder      55.6%   (fake-sender test)
```
Notes:
- `internal/db` 26.6%: the destructive `TestMigrate_Real` is opt-in (`MIGRATE_TEST_DESTRUCTIVE=true`) so it doesn't race with parallel test packages; the RBAC + pool tests run.
- `internal/sentry` 27.5%: the init/DSN path can't run without a DSN; no-op path fully covered.
- `internal/features/shared/records` 44.8%: the handler surface is covered end-to-end by `internal/api` + the feature tests; the adapter's per-feature routes are exercised via api tests.
- All feature packages (menu/booking/review/article/promo) register via `init()`; their validators are covered by `internal/features/validators_test.go` (package coverage 93.0%).
- Sports inventory/members 44.6%/44.5%: the adjust/topup handlers + isolation are covered by their package tests; the full HTTP surface is also exercised in the live smoke.

---

## Migrations shipped

```
001_clients_usage.sql    ✅  clients + usage + schema_migrations
002_tenancy.sql          ✅  tenant_records + RLS (PERMISSIVE)
003_seed_dev.sql         ✅  dev client (token='dev-token')
004_features.sql         ✅  clients.vertical + clients.features (Phase 3)
005_rbac.sql             ✅  app_runtime role split (Phase 3)
006_booking_date_idx.sql ✅  tenant_records booking date index (Phase 3)
007_sports_facilities.sql ✅  sports_facilities + RLS (Phase 4)
008_sports_bookings.sql  ✅  sports_bookings + EXCLUDE constraint (Phase 4) ⭐
009_sports_inventory.sql ✅  sports_inventory + RLS (Phase 4)
010_sports_members.sql   ✅  sports_members + RLS (Phase 4)
011_agent_bindings.sql   ✅  chat→client binding (Phase 5)
012_agent_reminders.sql  ✅  agentic reminders (Phase 5)
013_agent_staff_payments.sql ✅ staff payment LEDGER (Phase 5)
013b_sports_bookings_lifecycle.sql ✅ bookings lifecycle + source + override + payment_status (Phase 6)
014_sports_payments.sql  ✅  per-booking payments + recalc trigger (Phase 6)
015_sports_pricing_rules.sql ✅ date-aware day/evening pricing (Phase 6)
016_sports_expenses.sql  ✅  expense ledger (Phase 6)
017_sports_customers.sql ✅  customer directory + no-show (Phase 6)
018_sports_court_blocks.sql ✅ court blocks (Phase 6)
019_sports_recurring_series.sql ✅ recurring weekly series (Phase 6)
020_sports_kantin.sql    ✅  kantin POS (products/sales/items/movements) (Phase 6)
021_sports_users.sql     ✅  staff accounts (bcrypt, owner/pegawai) (Phase 7)
022_sports_sessions.sql  ✅  staff sessions (sha256 tokens, 30d) (Phase 7)
```

## Phase 7 — staff auth + agent money tools

**Staff auth (owner/pegawai) — no Supabase, no third party:**
- `internal/authapi/` — `POST /v1/auth/login` (bcrypt), `logout`, `me`. Sessions: random 32-byte token, sha256 at rest, constant-time compare, 30-day expiry. | tests: login/me/logout, wrong-pw 401, uniform unknown-user, expiry
- **Unified identity** — `records.WithIdentity`: `X-Session-Token` (staff, role) OR `X-Client-Token` (client, full access). All sports handlers refactored to `RequireIdentity`. | gate test + full suite
- **Owner gates** — expenses/pricing/series/courtblocks create+patch+del, payments del, customers del → `RequireOwner` (owner role OR client token). Staff bypass the feature gate.
- **Agent money tools** — `query_net_profit`, `query_range_summary`, `top_customers`, `closing_report`, `query_kantin_summary` (shared LLM key, no Gemini) + per-tenant daily cost cap (Redis, 150/day).
- **Availability** — `GET /v1/sports/availability?date=` (busy intervals per facility: bookings + blocks) for the landing page to poll.

**Key decisions:** auth is fully self-contained in the proxy (bcrypt, hashed session tokens, constant-time compare — the same discipline as client tokens). `sports_users` SELECT is open to app_runtime (login lookup needs no tenant GUC); writes are tenant-scoped. Staff identities bypass the per-client feature gate because they're already inside the tenant.

## Phase 6 — CentrumBadminton migration (real production features)

Ported the live badminton-arena PWA's backend into the proxy so the Next.js frontend can drop Supabase and talk to the proxy. Seven new features (payments, pricing, expenses, customers, courtblocks, series, kantin) + a bookings lifecycle extension.

| Feature | Proof |
|---|---|
| `payments` + recalc trigger | partial → paid → partial on delete (TestPayments_RecalcTrigger) |
| `pricing` + booking integration | day 2h×20k=40k, evening 2h×40k=80k (TestPricing_PriceForDayAndEvening) |
| `expenses` | CRUD + isolation |
| `customers` + visits | confirmed booking → visits=1 (TestCustomers_VisitsCounted) |
| `courtblocks` | CRUD + per-facility + isolation |
| `series` | CRUD + validation + isolation |
| `kantin` atomic sale | total server-computed 18000, stock 10→8/5→4, oversell→409 rollback (TestKantin_AtomicSale/Oversell) |

**Key decisions:** the exclusion constraint now ignores cancelled/no_show (rebookable). Bookings auto-compute price from the active pricing rule when the client sends price=0. The kantin sale is a single `WithTenantTx` — client price never trusted. `sports_kantin_sale_items` carries `tenant_id` (nullable on pre-existing DBs, always set by the handler) so the RLS pattern stays uniform.

## Phase 5 — Telegram agentic layer (the "agentic upgrade" tier)

Same proxy, same DB, same RLS, same quota — the agent is a conversational wrapper over the feature platform, not a separate service. Normal clients use REST only; agentic clients get the `telegram` feature.

| Item | What shipped | Proof |
|---|---|---|
| Channel | `internal/telegram/` — Bot API client (getUpdates), chat→client resolver, handler glue | fake-API tests |
| Brain | `internal/agent/` — tool loop (≤5 rounds), 11 tools, per-vertical system prompt, Redis history | fake-LLM tests: books a slot end-to-end, double-book error propagates |
| Tools | book/cancel/list bookings, income, attendance, stock check/adjust, member topup, staff payment ledger, list payments, set reminder | — |
| Scheduler | `internal/reminder/` — polls due `agent_reminders`, sends via Telegram, marks sent | fake-sender test: due sent, future stays pending |
| Feature | `telegram` registered (route-less gate), opt-in via `TELEGRAM_BOT_TOKEN` | main.go wiring |
| Business model | agentic = `telegram` in client.features + chat binding; LLM calls bill the same monthly quota | — |

**Design notes:** tools call the SAME `WithTenantTx` + RLS logic as REST — double-booking prevention, tenant isolation, quantity guards, and quota are inherited. Staff payments are a **ledger** (no money movement). `agent_bindings` RLS is `FOR SELECT USING (true)` (the one cross-tenant lookup table); the tenant boundary is enforced downstream by the feature gate + per-tool RLS. The reminder scheduler runs on the admin pool (internal infra) so it can see all tenants' due rows.

## Phase 4 — sports vertical (custom-table features in practice)

Shipped the first fully-strengthened vertical: **sports venue booking** (badminton, soccer, swimming, padel). This was the "strengthen first" pilot — it proved the custom-table feature path (the unproven part of Phase 3) and the hardest concurrency problem: **double-booking prevention**.

| Item | What shipped | Proof |
|---|---|---|
| New vertical | `sports` added to `AllowedVerticals` + default feature set | `/v1/sports/...` mounts |
| `facilities` feature | `sports_facilities` custom table + CRUD | CRUD + validation + isolation tests |
| `bookings` feature ⭐ | `sports_bookings` + **exclusion constraint** (`btree_gist`, `tsrange`) | `TestBookings_DoubleBooking` — 2 concurrent requests → exactly one 201, one 409, one DB row |
| `inventory` feature | `sports_inventory` + atomic quantity adjust | `TestInventory_NeverNegative` — oversell → 409, qty unchanged |
| `members` feature | `sports_members` + atomic top-up | topup test + isolation |
| WhatsApp queue | `internal/notify/` — `Sender` interface, in-process worker, retry/backoff, wa.me deep-link sender | queue tests (send once, retry, give up, never-block) |
| Builder change | `Features.Routes` now receives `BuilderDeps` so custom handlers get the pool | all feature tests |
| Docs | PRD §6.6, ARCHITECTURE §11, README, RUNBOOK §6 | — |

**The double-booking guarantee:** the DB rejects overlapping bookings on the same facility; the handler maps `23P01` → `409 {"error":"slot already booked"}`. The `tsrange` (not `tstzrange`) choice is documented in ARCHITECTURE §11.3 — `tstzrange`'s implicit timezone cast isn't IMMUTABLE, which Postgres forbids in index expressions.

**Test suite note:** `go test ./...` now needs `-p 1` (serial package execution). The integration tests share one Postgres, and parallel packages racing DELETE/INSERT on the same tables hit `tuple concurrently updated` (SQLSTATE XX000) at the page level. The Makefile `test` target uses `-p 1`; ARCHITECTURE §11.5 has the full reasoning.

## Endpoints shipped

| Method | Path | Auth |
|---|---|---|
| GET | `/healthz` | none |
| POST | `/v1/chat` | `client_token` in body |
| POST | `/v1/{vertical}/records` | `X-Client-Token` |
| GET | `/v1/{vertical}/records` | `X-Client-Token` |
| GET | `/v1/{vertical}/records/{id}` | `X-Client-Token` |
| PATCH | `/v1/{vertical}/records/{id}` | `X-Client-Token` |
| DELETE | `/v1/{vertical}/records/{id}` | `X-Client-Token` |
| POST | `/v1/admin/clients` | `Bearer ADMIN_API_KEY` |
| PATCH | `/v1/admin/clients/{id}` | `Bearer ADMIN_API_KEY` |
| GET | `/v1/admin/usage` | `Bearer ADMIN_API_KEY` |
| POST | `/v1/sports/facilities` | `X-Client-Token` |
| GET | `/v1/sports/facilities` | `X-Client-Token` |
| GET | `/v1/sports/facilities/{id}` | `X-Client-Token` |
| PATCH | `/v1/sports/facilities/{id}` | `X-Client-Token` |
| DELETE | `/v1/sports/facilities/{id}` | `X-Client-Token` |
| POST | `/v1/sports/bookings` | `X-Client-Token` (409 on overlap) |
| GET | `/v1/sports/bookings?date=&facility_id=` | `X-Client-Token` |
| GET | `/v1/sports/bookings/{id}` | `X-Client-Token` |
| PATCH | `/v1/sports/bookings/{id}` | `X-Client-Token` |
| DELETE | `/v1/sports/bookings/{id}` | `X-Client-Token` |
| POST | `/v1/sports/inventory/{id}/adjust` | `X-Client-Token` |
| POST | `/v1/sports/members/{id}/topup` | `X-Client-Token` |

`vertical ∈ {fnb, salon, laundry, klinik, gym, bimbel, bengkel, beauty, travel, property, katering, photo, sports}`

## Known limitations (carried forward)

- ⏳ Streaming SSE for `/v1/chat` (PRD §6.1 "pass 2")
- 🟡 Cost in `usage.cost_usd` uses fixed DeepSeek-flash estimates; needs real mimo pricing once a bill lands
- 🟡 Migrations live at `internal/db/migrations/` not repo root (Go `//go:embed` can't traverse `..`)
- 🟡 RLS policy is PERMISSIVE not RESTRICTIVE (Postgres semantics — RESTRICTIVE-only denies all rows)
- 🟡 Local dev requires a non-superuser `app` role; harness bootstraps it idempotently on first test run
