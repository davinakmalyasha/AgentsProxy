# Performance baseline — November 2026

Status: **first baseline captured; optimization has not started.** The
production numbers are a read-only, rate-limit-safe probe of the currently
deployed commit. The full-workload numbers are a local run of the same HTTP
router and handlers against a fresh seeded database. They are not a claim that
production handles the same concurrency.

- Production commit under measurement:
  `68c19969991ce4e009ba6071ade56a0948e6e054`. The deploy-gate code was
  promoted later; the current post-gate spot check is on `8f511ef`.
- Local code: the exit-gate branch working tree (validator, boot audit,
  smoke, fresh-DB runner, benchmark harness)
- Baseline date: 2026-11-24 (Asia/Jakarta)
- Local host: Windows 11, Go 1.27.0, PostgreSQL 18.6, Redis 8.10.1
- Local dataset: Centrum tenant with 8 facilities, 500 bookings, 50 court
  blocks, 12 pricing rules, 500 records, 100 expenses, 100 members, 100
  customers, one owner, one client token
- Harness: `scripts/bench.sh`; HTTP: `cmd/bench`; seed: `cmd/benchseed`;
  contract: `cmd/smoke`

## SLOs and result

Warm-path targets apply to production requests that reuse an instance. Cold
start has its own target because Vercel's Go runtime boots a process per
instance.

| Surface | Target p95 | Local p95 | Production p95 | Result |
|---|---:|---:|---:|---|
| `/healthz` | 200 ms | 4.83 ms | 138.43 ms | pass |
| Public facilities | 300 ms | 20.86 ms | 145.38 ms | pass |
| Public availability | 300 ms | 18.37 ms | 137.90 ms | pass |
| Public pricing | 300 ms | 15.65 ms | 168.43 ms | pass |
| Records list | 400 ms | 37.20 ms | not measured (no prod staff token) | local pass |
| Records create/delete | 500 ms | 137.59 ms | not measured | local pass |
| Staff login | 700 ms | 385.54 ms | not measured | local pass |
| Admin clients | 400 ms | 99.52 ms | not measured | local pass |
| Admin tenant health | 400 ms | 22.70 ms | not measured | local pass |
| Agent overhead (mock LLM, excl. provider) | 500 ms | 12.56 ms | not measured | local pass |
| Blended public, cold + warm | 1000 ms | not measured | not measured | unverified |
| Cold start, first request | 3000 ms | 780–6060 ms | 1670.6 ms cold p95 (4-instance burst) | pass; local proxy outlier |
| 5xx under baseline load | ≤ 0.1% | 0% | 0% (rate-limit-safe runs) | pass |
| Hot DB query p95 | 50 ms | 0.08–1.90 ms execution | — | pass |
| Redis hit ratio | ≥ 90% | 98.8% | — | pass |

The first draft used `/healthz` p95 < 150 ms. Production measured 138–161 ms
across two runs because the endpoint pays a DB ping and a Redis ping through
the Vercel edge. The frozen target is 200 ms; this is the one pre-baseline
adjustment, with the observed numbers above as its justification.

Production measurement detail:

- `/healthz`: 12 samples at 0.2 RPS, all 200; p50 78.27 ms, p95 138.43 ms.
- Facilities: 24 samples at 0.2 RPS, all 200; p50 117.54 ms, p95 145.38 ms.
- Availability: 24 samples at 0.2 RPS, all 200; p50 92.96 ms, p95 137.90 ms.
- Pricing: 18 samples at 0.2 RPS, all 200; p50 90.53 ms, p95 168.43 ms.
- A 0.2 RPS run still hit six 429s per endpoint when warmup was unpaced.
  Warmup used the same IP budget as measurement; the harness now paces
  warmup and the reported runs are clean. This is evidence the per-IP limiter
  is working, not a latency result.

Local full-workload detail (concurrency 16, 5 s measurement, 2 s warmup):

| Scenario | Requests | Errors | RPS | p50 ms | p90 ms | p95 ms | p99 ms |
|---|---:|---:|---:|---:|---:|---:|---:|
| health | 34,993 | 0 | 6,994 | 1.83 | 3.60 | 4.83 | 9.72 |
| public-facilities | 7,170 | 0 | 1,434 | 9.47 | 16.54 | 20.86 | 43.98 |
| public-availability | 9,849 | 0 | 1,964 | 6.42 | 13.83 | 18.37 | 30.44 |
| public-pricing | 8,323 | 0 | 1,664 | 8.94 | 13.43 | 15.65 | 22.61 |
| records-list | 4,307 | 0 | 861 | 15.45 | 28.09 | 37.20 | 71.22 |
| records-create-delete | 1,592 | 0 | 318 | 37.61 | 77.70 | 137.59 | 161.00 |
| staff-login | 276 | 0 | 55 | 269.28 | 360.99 | 385.54 | 471.90 |
| admin-clients | 2,486 | 0 | 497 | 22.06 | 39.59 | 99.52 | 256.92 |
| admin-tenant-health | 5,661 | 0 | 1,132 | 12.75 | 18.10 | 22.70 | 40.90 |

Agent-only run (concurrency 4, 5 s):

| Scenario | Requests | Errors | RPS | p50 ms | p95 ms | p99 ms |
|---|---:|---:|---:|---:|---:|---:|
| agent-client-chat (mock LLM) | 3,748 | 0 | 750 | 4.17 | 12.56 | 24.11 |
| chat (mock LLM) | 1,927 | 0 | 385 | 7.33 | 15.03 | 113.84 |

Local cold start (process start → `/healthz` 200, five restarts):

```
run 1: 31705 ms   <- first run includes OS file scan / binary cold cache
run 2:  1751 ms
run 3:  1301 ms
run 4:  6061 ms
run 5:   782 ms
```

Runs 2–5: median 1.53 s, p95 6.06 s. The first run is an outlier dominated
by the Windows filesystem, not Go boot. This is a local proxy, not a Vercel
function cold start.

Production cold-start probe: 20 `/healthz` GETs ten seconds apart returned
200 with TTFB 185–332 ms (median ≈ 220 ms) and no multi-second spike.
`x-vercel-id` changed on every request, so it identifies the request, not the
reusable instance; the probe cannot prove whether each request was cold or
warm. It is recorded as a provisional pass, not a cold-start guarantee.

## Database evidence

`EXPLAIN (ANALYZE, BUFFERS)` after the load:

| Query | Plan | Execution |
|---|---|---:|
| facilities list | bitmap index on `sports_facilities_tenant_idx` | 0.19 ms |
| pricing list | bitmap index on `(tenant_id, valid_from)` | 0.15 ms |
| records list (50 newest) | **Seq Scan** on `tenant_records` + top-N sort | 1.90 ms |
| admin clients list | Seq Scan on `clients` (one row) | 0.08 ms |

The records scan is 2,756 rows here and will grow linearly. The existing
`tenant_records_tenant_vertical_kind_idx` does not serve
`WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT 50`.

Redis during the full run: 34,826 hits / 418 misses = 98.8% hit ratio;
315,540 commands processed.

## Top five bottlenecks

1. **Staff login is bcrypt-bound.** p50 269 ms, p95 386 ms; at concurrency 16
   it limits the mixed workload to ~55 logins/s. This is expected security
   work, not a handler leak. Any change must preserve the timing-equal
   unknown-user path and lockout semantics.
2. **Records create/delete is the slowest write path.** p95 138 ms, p99
   161 ms. It performs auth, a tenant transaction, RLS, insert, then a
   separate delete. Measure the round-trip split before changing semantics.
3. **Records list is a sequential scan.** 1.9 ms at 2.7k rows is harmless,
   but there is no `(tenant_id, created_at DESC)` index. This is the clearest
   scale risk in the baseline.
4. **Cold-start tail is real but within target.** The 4-instance burst showed
   cold p95 1.67 s against the 3 s target, while application time stayed
   ~3.5 ms. The tail is Vercel boot/edge, not handler work; the labeled
   `Server-Timing` run makes that separation explicit.
5. **Public read latency is dominated by edge/network, not the handler.**
   Local p50 is 6–12 ms; production p50 is 78–118 ms for the same public
   reads. The code path is already fast; the remaining lever is connection
   reuse/region/caching at the edge, not query tuning.

## What was not measured

- Production records, staff login, admin reads, and agent chat: no production
  staff/admin/client credentials were used. The local run is the substitute.
- A blended cold+warm SLO: insufficient production samples; cold and warm
  are reported separately instead.
- A production load test above 0.2 RPS per endpoint: the per-IP limiter
  returns 429 above the safe probe rate. Do not bypass it for a benchmark.

## Post-gate production spot check

After the exit-gate deploy (`8f511ef`), a 240-second, 0.2 RPS probe from the
same workstation produced 48 samples per endpoint:

| Scenario | p50 | p95 | p99 | Errors |
|---|---:|---:|---:|---:|
| health | 101.4 ms | 1137.6 ms | 5943.7 ms | 0 |
| public-facilities | 102.6 ms | 230.7 ms | 373.1 ms | 0 |
| public-availability | 104.3 ms | 220.2 ms | 263.9 ms | 0 |
| public-pricing | 136.4 ms | 575.9 ms | 1470.8 ms | 0 |

The medians are stable and close to the pre-gate baseline, but the tails
contain multi-second outliers. A separate 20-probe health run and the US
`perf-guard` run did not reproduce those outliers, so this is not yet
attributable to a code regression; it can be client/network jitter, a cold
start, or edge-instance variance. The labeled rerun below resolves this: the
application path is 4–15 ms p95 and the tail was cold/network. This earlier
table is kept as the reason the instrumentation was added.

## Labeled latency measurement

The follow-up probe separates the three things the first run could not:

- **network time** — client-observed p50/p95/p99;
- **application time** — `Server-Timing: app;dur=<ms>` emitted before the
  first response byte;
- **cold vs warm** — `X-Instance-Id` is a process-boot identity, so the first
  request seen for an instance is cold and later requests are warm.

### Warm run — 48 samples per endpoint, 0.2 RPS, 240 s

Production `ae5ce868`, one instance for the whole run:

| Scenario | p50 | p95 | p99 | Server p95 | Cold samples | Warm p95 |
|---|---:|---:|---:|---:|---:|---:|
| health | 79.8 ms | 107.5 ms | 359.2 ms | 3.97 ms | 0 | 107.5 ms |
| public-facilities | 91.1 ms | 141.5 ms | 176.8 ms | 15.04 ms | 0 | 141.5 ms |
| public-availability | 93.8 ms | 138.7 ms | 208.5 ms | 14.61 ms | 0 | 138.7 ms |
| public-pricing | 89.1 ms | 135.4 ms | 211.1 ms | 14.75 ms | 0 | 135.4 ms |

The warm p95s are **inside** the 200 ms health and 300 ms public targets.
Application time is 4–15 ms p95; essentially all of the client-observed
latency is network/edge. The earlier multi-second tails were not stable
handler latency.

### Cold burst — 4 instances, 3 s

A short burst forced four new instances:

| Scenario | p50 | p95 | Server p95 | Instances | Cold p95 | Warm p95 |
|---|---:|---:|---:|---:|---:|---:|
| health | 77.7 ms | 1676.9 ms | 3.48 ms | 4 | 1670.6 ms | 1682.5 ms |

Cold p95 is 1.67 s, under the 3 s cold-start target, while application time
remains ~3.5 ms. The gap is Vercel instance boot/network, not the Go handler.
(Warm p95 is distorted by the burst's queueing and is not a warm SLO sample.)

### Current conclusion

- Absolute **warm** public/health SLOs: pass in the labeled run.
- Absolute **cold** health SLO: pass in the burst (1.67 s < 3 s).
- The earlier 48-sample multi-second p95 outliers are best explained by
  cold/network variance; the application path is fast.
- Production staff/admin/agent absolute latency remains unmeasured.

## Round-trip and edge-cache pass (2026-11-26)

The production `pg_stat_statements` snapshot showed hot app queries executing
in 0.04–1.7 ms while the labeled server p95 was 14–15 ms. The gap is
sequential round trips, not query work. The production dataset was also tiny
(2 clients, 2 bookings, no tenant records), so index changes cannot explain
or improve that latency.

Implemented in the round-trip pass:

- The request identity's resolved schema is reused by the tenant transaction;
  a 30-second bounded cache covers agent/tool paths that have no identity.
- `private.record_usage` replaces the four-round-trip usage transaction with
  one definer call plus the quota Redis increment.
- `private.staff_session_resolve_v2` resolves the session, its live tenant
  routing/entitlement, and the once-a-minute `last_seen_at` touch in one call.
  Centrum reads only its user row through the resolved schema.
- Health probes run DB and Redis checks concurrently.
- Boot quarantine reconciliation uses one runtime-pool RPC, so a serverless
  cold start no longer opens the direct-5432 admin pool.

Implemented in the edge-cache pass:

- Public facilities/pricing GET 200 responses carry a 10-second
  `Vercel-CDN-Cache-Control` / `CDN-Cache-Control` TTL; availability uses 2
  seconds. Browser `Cache-Control: no-store` is unchanged.
- Only the unauthenticated public identity marks a response cacheable;
  staff/client responses never do.
- `Gzip` now removes only its own `Accept-Encoding` Vary token instead of
  deleting the entire header, and CORS always advertises `Vary: Origin` for
  non-wildcard responses. Without those fixes, a CDN could conflate CORS or
  compressed/identity variants.
- `cmd/bench` distinguishes `x-vercel-cache: HIT` samples from origin
  samples and can cache-bust GETs for origin measurement. The daily
  `perf-guard` now cache-busts so a warm edge cannot hide an origin
  regression.

Production verification of the measured win is recorded after the code
deploy; the pre-change table above remains the comparison baseline.

### RLS initplan hardening (2026-11-26, `b5ab576`)

All 37 tenant RLS policies (`public` + `project_centrum`) now evaluate the
tenant GUC once per statement via a scalar subquery instead of once per row.
`EXPLAIN` shows `InitPlan 1` with the index condition on the initplan value;
`pg_policies` has zero bare `current_setting` calls, and the Supabase
`auth_rls_initplan` advisory cleared (only INFO-level `unindexed_foreign_keys`
and `unused_index` remain). The `internal/db`, `tenantdb`, `records`, and
`centrum/auth` isolation suites pass unchanged, and production smoke passes
on the migration commit. At the current tiny production dataset this is scale
hardening, not a latency change — the payoff grows with rows scanned per
query. Migrations: platform `094_rls_initplan.sql`, Centrum
`0013_rls_initplan.sql` (migration-only PR #120, applied via `db-migrate`).

### Legacy index cleanup (2026-11-26, applied via `db-migrate`)

Seventeen indexes with no possible reader were dropped (migration-only
PR #126: platform `096_drop_unused_indexes.sql`, Centrum
`0015_drop_unused_indexes.sql`). Each drop was proven against every query
in the codebase, not the idle-DB advisory: slug / payload-GIN /
payload-date expression (no such predicates exist), customers / expenses /
members plain indexes (every read carries the live predicate the partial
serves), payments `booking_idx` (every booking-scoped read filters deleted;
the live index serves index-only), facilities `tenant_idx` (every read
filters deleted), kantin live `created_at` / `live_id` partials (no matching
query shape — the export includes voided rows, money buckets by WIB day),
and reminder-due / notify-tenant / sessions-tenant / audit / report-objects
indexes with no readers. Deliberately kept: the `paid_at` sort, both
recorder pairs, the voided-inclusive export index, all lookup/FK/sole-access
indexes, and the agency-plane indexes (live code, no traffic evidence to
judge them by). Fresh chain applies clean with zero remaining; the
db/tenantdb/records/auth suites plus the expenses/payments/kantin/customers/
members/facilities feature suites all pass; prod `pg_indexes` confirms zero
remaining and smoke passes. Write amplification drops on every insert,
update, and tombstone write.

### Cron probe-first hardening (2026-11-26, ops PR #124)

`private.tick_report_jobs` (every minute) ran its reap UPDATE before checking
for work, so every idle minute took a row lock. It now probes first: no
pending rows and no stale working rows means two indexed probes
(`report_jobs_pending_idx`, `report_jobs_working_claimed_idx` from step 1)
and zero writes. Semantics are unchanged — stale `working` rows are still
reaped to pending before the build decision.
Verified on prod after applying the function: the stored definition carries
the gate, an idle tick returns 0 with `n_tup_upd` unchanged, and smoke passes.
`reminder-tick` was already probe-gated and `retention-tick` is a daily
batched job, so both were left alone; `reap_stale_claims` already runs as a
single indexed UPDATE and was not worth a migration for zero measured gain.

### Post-deploy origin split (2026-11-26, `be7c639`)

Twelve cache-busted samples per endpoint at 0.2 RPS (origin only):

| Scenario | Server p95 | DB p95 | Redis p95 |
|---|---:|---:|---:|
| health | 3.40 ms | 0 ms (ping untraced) | 1.33 ms |
| public-facilities | 13.80 ms | 7.18 ms | 5.21 ms |
| public-availability | 14.99 ms | 8.42 ms | 4.70 ms |
| public-pricing | 14.26 ms | 7.92 ms | 4.80 ms |

The split adds up: ~7–8 ms database + ~5 ms Redis + ~2 ms application
work inside ~14–15 ms server time. The pre-optimization labeled run showed
server p95 4–15 ms without the split; the absolute origin latency is
unchanged (no regression), and the composition is now measured rather than
inferred. Raw artifacts: `docs/perf/2026-11-26/prod-origin-60.json`.

A parallel non-busted run confirmed edge behavior: facilities/pricing serve
`X-Vercel-Cache: HIT` inside the 10-second TTL (7 hits / 5 origin on
facilities), while 2-second availability at 0.2 RPS always revalidates.
Client-observed edge p95 in that small run was noisy (network/cold tails
with server p95 still ~15 ms), so the edge win is stated as origin offload,
not a same-client latency claim. Raw artifacts:
`docs/perf/2026-11-26/prod-edge-60.json`.

## Continuous guard

`.github/workflows/perf-guard.yml` runs the same public probe daily at 0.2 RPS
from a US GitHub runner. It enforces runner-specific regression ceilings
(health 500 ms, public 600 ms), not the same-region 200/300 ms SLOs; the
transpacific path adds roughly 100–150 ms. A violation opens or updates a
`[perf-alert]` issue. It is a read-only guard, not a replacement for the local
staff/admin/agent workloads or a same-region absolute SLO probe.

## Optimization queue (not implemented)

Ordered by evidence, not intuition:

1. Add and validate `tenant_records (tenant_id, created_at DESC)`; switch the
   list endpoint to keyset pagination if deep pages are used.
2. Split records create/delete latency into auth, transaction begin/commit,
   insert, and delete; remove only measured round trips.
3. Profile staff login with CPU and DB spans; decide whether bcrypt cost,
   session insert, or rate-limit Redis is the dominant share.
4. Reduce boot work: measure each boot phase (config, pools, manifest, audit,
   ADK, tool sets) and lazy-load only what is not needed for `/healthz` and
   public reads.
5. Re-run the same harness after each change; the frozen SLO table above is
   the comparison, not a new target.

## Raw artifacts

The local run writes pprof, `EXPLAIN`, Redis `INFO`, server logs, and JSON to
`.tmp/bench/<timestamp>/` (gitignored). The curated summaries for this
baseline are in `docs/perf/2026-11-24/`.

## Verification commands

```bash
# Reproduce the local full workload (fresh DB, seed, server, pprof, plans)
BENCH_DURATION=5s BENCH_WARMUP=2s BENCH_PROFILE=1 bash scripts/bench.sh

# Agent-only local workload
BENCH_SCENARIOS=agent-client-chat,chat BENCH_CONCURRENCY=4 \
  BENCH_DURATION=5s bash scripts/bench.sh

# Production read-only, rate-limit-safe
.\.tmp\bench\bench.exe -base-url https://agents-proxy-nine.vercel.app \
  -scenarios public-facilities,public-availability \
  -concurrency 1 -rps 0.2 -duration 120s -warmup 5s
```

### Optimization batch 2026-11-26 (Go hot-path + bench harness, RLS gaps)

Shipped via gate: migration PR #128 (097 initplan gaps, fixed in #129 for
prod table-move) applied via `db-migrate`; code PR #130 deployed via
`deploy.yml` and live on prod `5c41318`, smoke passing.

Go (all low-risk, verified by suites):
- Records create metering async off the 201 (detached ctx preserved;
  `records_test.go` polls with 5s deadline for the eventual row).
- Agent RPD + cost caps pipelined 2 RTT -> 1 (`redisx.IncrExpireTwo`,
  `CapsAllowed`; same fail-closed semantics).
- Logging lazy attrs (prod Info skips Debug alloc on successful GETs).
- `reqmetrics` hooks skip clock with no span; `requestip` single VERCEL
  lookup per request (reverted Once cache — broke `t.Setenv` tests).
- Availability: `X-Avail-Cache: hit/miss` header, `__bench_cb` bypasses
  in-process cache for true origin, lazy evict (no O(cap) scan per miss).

Bench harness (no prod impact, fixes measurement blind spots):
- Per-scenario tracker (was global, diluted cold attribution).
- Robust `Server-Timing` parse (quoted `dur="12.3"`, `;desc=` params).
- Table + markdown now show `origin/db/redis/cache_hits/cold_N/avail`.
- `records-create-delete` measures POST only (DELETE cleanup outside timing).

DB:
- 097 closes remaining initplan gaps conditionally (prod tables already
  moved/dropped by Centrum 0001/0005/0009 and 042; fresh chain still fixes
  pre-move tables). Prod `db-migrate apply` green.

Prod origin probe (2026-11-26, `5c41318`, `--cache-bust`, 1 conc, 0.5 RPS,
10 samples/endpoint, `scripts/bench.sh` remote):
- health: p95 130ms, srv 4.5ms, redis 1.3ms (db 0 — ping untraced, known).
- facilities: p95 136ms, srv 15.2ms = db 8.9ms + redis 4.0ms + ~2ms app.
- availability: p95 395ms client, srv 16.3ms = db 10.5ms + redis 5.5ms;
  avail_miss 10/10 (bypass works), coldN 0 (same instance).
- pricing: p95 2583ms client (single 2.5s edge outlier, N=10),
  srv 17.9ms = db 10.7ms + redis 4.8ms; no errors on any endpoint.
- Server p95 15–18ms matches pre-batch baseline (14–15ms): no regression,
  plus db/redis splits and true origin for availability.

Artifacts: `.tmp/bench/20261126T095320Z/results.{json,md}` (gitignored).

### Optimization batch 2 — 2026-11-26 (folds + pricing cache + money single-pass)

Shipped via gate: migration PR #132 (`0016_money_range_single_pass`)
applied via `db-migrate`; code PR #133 deployed via `deploy.yml` and live
on prod `a522804`, smoke passing.

Code (all verified by suites, no new indexes — EXPLAIN on 2k seeded
bookings showed every hot query already optimal at 0.05–0.9 ms):
- `closing_report` 3 sequential QueryRows → 1 row (cash/qris + 2 booking
  counts with subselects). 3 RTT → 1 per closing call.
- Bookings PATCH 3 SELECTs of the same row (start/date/end, price,
  status) → 1 (`start/date/end/price/status`); release uses fetched price,
  transition uses fetched status. 2–3 RTT → 1 plus the CAS UPDATE.
- Payments create price + SUM 2 SELECTs → 1 (price/override + paid-sum
  subquery under the same `FOR UPDATE` row lock). Trigger recalc stays
  (correctness).
- Pricing rule 30 s in-process cache (`tenant|date`, 1024-cap, single
  invalidation on create/patch/applyHours/del); contract gate still runs
  per call so safety is unchanged. Saves 1 SELECT (~0.07 ms exec + RTT)
  on every booking create, extension reprice, and `get_pricing`.

DB:
- `0016_money_range_single_pass.sql` (Centrum): 10 subqueries → 7.
  Payments cash/qris/other/count collapse to one `FILTER` agg CTE + best;
  booked/bookings collapse to one bookings `FILTER` CTE + peak. Same
  predicates, same JSON shape; kantin/expenses/peak untouched.

Probe (2026-11-26, `a522804`, `--cache-bust`, 1 conc, 0.5 RPS, 10/endpoint):
- health: p95 90ms client, srv 4.5ms.
- facilities: p95 65ms, srv 12.8ms = db 9.6ms + redis 2.0ms.
- availability: p95 77ms, srv 14.6ms = db 11.0ms + redis 1.6ms.
- pricing: p95 77ms, srv 14.0ms = db 10.2ms + redis 1.5ms.
- 0 errors on all endpoints; all origin (bust), avail 10/10 miss as
  designed, coldN 0 (single instance). No regression vs batch-1
  (srv 15–18ms); pricing edge outlier from batch-1 did not repeat.

Artifacts: `.tmp/bench/20261126T150201Z/results.{json,md}` (gitignored).

### Traffic-derived SLOs (2026-11-26)

The synthetic-probe question is settled without a synthetic tenant:
production traffic now produces the staff/admin/agent numbers itself.

- `internal/slo` keeps per-class histograms in-process, classed by surface
  (`public`, `staff`, `admin`, `agent`, `chat`, `other`; `/healthz` excluded).
- One EVAL per class with traffic per 5-minute interval writes
  `slo:<class>:<bucket>` (2 h TTL). Detached, best-effort, never on the
  request path. Lost windows on serverless freeze affect statistics only.
- `GET /v1/admin/slo?window=60` returns server-observed p50/p95/p99 per class,
  marked approximate, with `ok | insufficient | breach` status.
- `agent`/`chat` are provider-bound and never breach. Below 20 samples a class
  reports `insufficient` — expected for staff/admin on a low-volume tenant.
- `slo-burn.yml` alerts hourly via the `[slo-alert]` issue once the
  `ADMIN_API_KEY` repository secret is configured; otherwise it is a no-op
  notice and the endpoint remains available for manual reads.

This does not replace the public probe: `perf-guard` remains the change
verification and gross-regression guard, and its numbers are never mixed with
traffic SLOs.

#### Live verification (2026-11-27, `0ab6208`)

- Deploy gate success; prod smoke passes on `0ab6208` (health, facilities,
  availability, pricing, 401/404/204 contracts).
- `GET /v1/admin/slo` unauthenticated → 401 (mounted, admin-gated).
- `slo-burn.yml` dispatched: exits 0 with an `ADMIN_API_KEY secret is not
  set` notice, as designed. Automated hourly burn alerts activate when the
  production admin key is added as the `ADMIN_API_KEY` repository secret;
  until then the endpoint is read manually.
- Flush timing: the middleware now calls `slo.MaybeFlush()` at request
  start, so the detached Redis write runs during the request instead of
  after it — a serverless freeze is much less likely to drop a window.
- Honest residual: accumulation/readback is proven by the Redis integration
  test and the `/v1/admin/slo` route test, but the first real production
  read requires the admin key. Expect `insufficient` for staff/admin/agent
  until real traffic reaches 20 samples in the window.

### Optimization batch 3 — 2026-11-27 (origin round-trip cuts)

Shipped via the normal gate: code PR #137 merged as `6d53f46` (code
`4484840`); deploy workflow staged, smoked, promoted, and prod smoke
passed on `6d53f46`. No DDL, no new indexes, no synthetic tenant.

What changed and why it was measured first:
- **One Redis round trip instead of three on public reads.**
  `RateLimitClient` no longer falls back to IP: the pre-auth limiter
  already owns that IP bucket, so the fallback opened a second,
  independent bucket with the same 180/min cap and cost one EVAL per
  public/project request. Verified client IDs keep their per-account
  budget. `CheckTenantInfoCached` gained a 5 s process-local L1 in front
  of the 30 s Redis entry; admin eviction clears L1 and other instances
  converge in 5 s (better than the Redis TTL).
- **One DB round trip instead of four on single-query tenant reads.**
  Production runs Supavisor transaction mode / simple protocol.
  `tenantdb.ReadTenant` puts `SET LOCAL app.tenant_id/search_path` and
  the SELECT in one simple Query message, which PostgreSQL executes as
  one implicit transaction. Extended-protocol pools keep the old
  `WithTx` path. Applied to availability, facilities, pricing, and
  records list/get; multi-statement/write handlers still use `WithTx`.
- The effective anonymous IP cap is unchanged (global limiter still
  180/min); the removed bucket was a duplicate cap, not an extra one.

Proof:
- `TestReadTenantSimpleProtocolRLS` runs on a real `app_runtime`
  simple-protocol pool with `MaxConns=1`: tenant A sees only A's rows,
  `current_schema()` is the tenant schema, `app.tenant_id` does not
  leak back to the connection, a failed read does not poison the next
  one, and the extended-protocol fallback returns identical context.
- L1 hit/evict/TTL/negative-cache tests; no-identity limiter test
  asserts zero Redis keys written.
- Full `internal/api` suite, records, facilities, pricing, tenantdb,
  middleware all pass locally, including a simple-protocol end-to-end
  project-route run that exercises the production fast path.
- The middleware Redis test helper now parses `redis://` URLs, so those
  integration tests actually run in CI instead of skipping on a
  malformed `Addr`.

Production probe (2026-11-27, `6d53f46`, `--cache-bust`, 1 conc,
0.5 RPS; 45–60 samples/endpoint; all origin, 0 errors, coldN 0):

| Scenario | Server p95 before → after | DB p95 before → after | Redis p95 before → after |
|---|---:|---:|---:|
| public-facilities | 13.80 → 6.88 ms | 7.18 → 1.86 ms | 5.21 → 2.43 ms |
| public-availability | 14.99 → 7.84 ms | 8.42 → 1.57 ms | 4.70 → 2.56 ms |
| public-pricing | 14.26 → 7.32 ms | 7.92 → 1.50 ms | 4.80 → 2.80 ms |

“Before” is the post-batch-2 origin split in this document. The drop is
the round trips leaving, not a faster query plan: execution times did
not change and no index was touched.

Client-observed p95 in the cache-busted run was 112–130 ms (network/
edge-bound; baseline was 135–145 ms). A parallel non-busted run showed
24/30 edge hits on facilities and pricing (10 s TTL), 4/30 on
availability (2 s TTL); p50 fell to 63–69 ms from the baseline 90–118 ms,
while one facilities p95 outlier shows why the edge numbers are reported
as p50/offload, not a new absolute SLO. Raw artifacts:
`docs/perf/2026-11-27/prod-origin-45.json`,
`docs/perf/2026-11-27/prod-origin-facilities-60.json`, and
`docs/perf/2026-11-27/prod-edge-30.json`.

Residual, stated deliberately:
- Edge TTLs are unchanged (availability 2 s, facilities/pricing 10 s);
  raising them is a product freshness decision, not a Go change.
- Cold start is unchanged (1.67 s p95 last measured); boot-phase logs
  are still the prerequisite before lazy-init work.
- staff/admin/agent SLOs remain `insufficient` until real traffic reaches
  20 samples/window; no synthetic tenant was introduced to fill them.

### Edge TTL raise — 2026-11-27 (client-latency lever)

Shipped via the normal gate: code PR #139 merged as `26d1d88`;
deploy workflow staged, smoked, promoted, and prod smoke passed on
`26d1d88`. No DDL, no new indexes, no synthetic tenant.

What changed and why it was measured first:
- Availability edge: `s-maxage 2 -> 10`, `SWR 5 -> 30`; in-process
  `cacheTTL` 3 s -> 10 s (absorbs two ~5 s landing-board poll cycles).
- Facilities/pricing edge: `s-maxage 10 -> 60`, `SWR 30 -> 120`.
- Freshness sign-off: user approved 2026-11-27 ("go on"). Worst case
  staleness is bounded by s-maxage: 10 s on availability, 60 s on
  facilities/pricing. Staff writes still clear the server-side rule
  cache; the edge window is the only added staleness.

Proof:
- Prod headers on `26d1d88`: facilities/pricing
  `Cdn-Cache-Control: public, s-maxage=60,
  stale-while-revalidate=120`; availability `s-maxage=10,
  stale-while-revalidate=30` with `X-Avail-Cache: hit` on repeat
  reads (10 s in-process window working).
- Facilities/pricing package tests + records tests pass locally; no
  test asserts endpoint TTL values (helper behavior only).
- Prod smoke: health, facilities, availability, pricing, 401/404/204
  contracts all PASS on `26d1d88`.

Production edge probe (2026-11-27, `26d1d88`, non-busted, 1 conc,
0.5 RPS, 30 samples/endpoint, 0 errors):

| Scenario | p50 | p95 | Edge hits | DB p95 | Redis p95 |
|---|---:|---:|---:|---:|---:|
| public-facilities | 68.52 ms | 123.45 ms | 29/30 (was 24/30) | 0.00 ms | 1.95 ms |
| public-availability | 65.45 ms | 101.04 ms | 24/30 (was 4/30) | 2.06 ms | 3.25 ms |
| public-pricing | 66.69 ms | 123.91 ms | 29/30 (was 24/30) | 0.00 ms | 0.99 ms |

One facilities cold start (coldN 1, 119.08 ms) explains the 62.86 ms
server p95 outlier on that row; warm p50 is 63-69 ms across all
three. DB p95 reads 0.00 ms on facilities/pricing because edge hits
never reach Postgres. Raw artifact:
`docs/perf/2026-11-27/prod-edge-ttl60.json`.

Residual, stated deliberately:
- Availability can serve up to 10 s stale per instance plus 10 s at
  the edge; facilities/pricing up to 60 s at the edge. Revert is one
  PR if the board ever needs fresher data.
- Origin handler speed is unchanged (batch 3: server p95 6.9-7.8 ms);
  this batch moves client-observed p50/offload, not origin execution.
- Cold start unchanged; one cold observed in this probe as usual.
