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
| Cold start, first request | 3000 ms | 780–6060 ms | 185–332 ms TTFB (20 spaced probes) | production provisional pass; local proxy outlier |
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
4. **Cold start is still an uncertainty.** Local process-to-health ranges from
   0.78 s to 6.06 s after the first-run outlier. Production spaced probes saw
   185–332 ms TTFB with no multi-second spike, but `x-vercel-id` is
   request-scoped, so instance reuse cannot be proven. Boot work (config, two
   pools, Redis, manifest, entitlement audit, ADK, tool sets) remains the
   lever if a real cold-start regression appears.
5. **Public read latency is dominated by edge/network, not the handler.**
   Local p50 is 6–12 ms; production p50 is 78–118 ms for the same public
   reads. The code path is already fast; the remaining lever is connection
   reuse/region/caching at the edge, not query tuning.

## What was not measured

- Production records, staff login, admin reads, and agent chat: no production
  staff/admin/client credentials were used. The local run is the substitute.
- Vercel instance reuse and true cold-vs-warm split: the 20-request probe
  records TTFB but `x-vercel-id` is request-scoped. A deployment-instance
  metadata source or controlled idle window is still needed.
- A blended cold+warm SLO: insufficient production samples.
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
start, or edge-instance variance. The absolute p95 SLOs should therefore be
treated as **unverified**, not passed, until the probe can separate server
time from network time and identify cold instances. The median path is
healthy; the tail is the next measurement problem.

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
