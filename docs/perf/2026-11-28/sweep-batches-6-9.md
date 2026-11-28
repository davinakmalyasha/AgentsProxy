# Last-sweep batches 6–9 (2026-11-28)

Five-subagent deep audit (Go path, DB, backend handlers, Redis/pool/edge,
chat/metering/SLO). Origin 7–15ms, edge offload ~100% — the sweep is tail
survival + correctness, not medians. All live, smoke PASS throughout.

## Batch 6 — correctness P0s (`8271238`, PR #161)

- `SLOClass`: `POST /v1/chat/stream` → `chat` (was `other`, pageable 500ms).
  Provider latency no longer pollutes the server SLO; `chat` counts whole.
- `LimitJSONBody` on `/v1` group: covers `/v1/push/*` + `/v1/auth/*`, which
  sit outside project/vertical builders. 1 MiB, GETs untouched.
- Skip `usage.Record` when `in+out==0` (3 chat meter sites): $0 row costs
  1 DB RPC + 1 Lua, quota counts tokens so unchanged. Volume stays in SLO.
- Pricing `ruleCacheClear` (global) → `ruleCacheEvictTenant`: one tenant's
  edit no longer cold-misses every tenant.

## Batch 7 — money-path RTT (`807e89f`, PR #162)

- Payments create + kantin createSale: idempotency pre-check Tx (~4 RTT)
  folded into the write Tx (members-topup shape, unique index arbiter,
  race fallback kept, closure retry-safe).
- Kantin `listSales`/`getSale`/`listSaleItems`: `WithTenantTx` →
  `WithTenantRead` (saves BEGIN/set_config/COMMIT each). Tenant check
  folded into the items JOIN; cross-tenant now 404 not 403 (no oracle,
  strictly safer).
- Declined: payments-patch triple-read collapse (would change FOR UPDATE
  lock order on a staff-only rare path; correctness over 2 in-Tx RTT).

## Batch 8 — resilience (`fd06cfc`, PR #163)

- `MarkPublicCache` += `stale-if-error=300`: error-path only, serves ≤5m
  stale instead of 500s on origin blips. Booking correctness server-side.
- `quota.Check` miss path behind singleflight (MonthKey, detached 5s ctx):
  kills month-rollover SUM stampede; deny-on-error unchanged.
- `resolveAuth` sanitizes model once (post-auth; idempotent).
- Boot `phase("tools_snapshot")` timer (attribution).

## Batch 9 — agent guards (`42ea508`, PR #164)

- `cachedSet` behind singleflight: concurrent cold turns share one MCP
  discovery + adaptation; failures not cached; generation check kept.
- `cachedPrompt` builds outside the write lock (double-checked, pure).
- slo-burn missing-secret `::notice::` → `::warning::` (still skips, now
  visible; no hourly issue spam).
- Declined: chat Allow+quota pipeline (provider-bound seconds, ~1–4ms
  noise, money-path coupling risk).

## Declined / deferred with reasons (do not re-propose without new evidence)

- Healthz middleware bypass: remaining cost is ~5 header sets + 2 allocs
  + 1 mutex per probe (noise); bypass diverges bench/monitor headers.
- Gzip lazy headers: rewrites a tested state machine (SSE/xlsx
  passthrough) for ~µs on small JSON.
- Pool-acquire bound + 429: real work (DeadlineExceeded→429 mapping in
  every handler); single-IP saturation already shed by limiters;
  multi-IP proof needs staging distribution test first.
- List-cap unification (2000 → 100/200): contract change for large tenants;
  no COUNT(*)/OFFSET exists anywhere in centrum (only agency back-office).
- RLS read definer (availability): only if EXPLAIN shows RLS >2ms;
  initplan-hardened, isolation risk otherwise.
- Third TTL notch: variance-dominated, doubles worst-stale; product sign-off.
- Tenant L1 / Lua-merged gate / payments 4→3: prior verdicts stand.
- Chat pipeline, patch collapse: declined above.

## Manual / product residuals (not code)

- Upstash region console check (assert `ap-southeast-1`; `redis;dur` 1–5ms
  says same-region today — 30-second read).
- `ADMIN_API_KEY` secret for burn alerts (warning now visible when unset).
- Frontend streaming integration (dfdagency, real tokens); third TTL call.
- Pool saturation curve needs distributed-source staging test (limiters
  block the single-IP version by design).
