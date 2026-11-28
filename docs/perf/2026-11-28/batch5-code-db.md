# Batch 5 — code + DB maintenance (2026-11-28)

Live: migration `6075ff1` (098 public + centrum 0017 ANALYZE), code `29c77b7`.
Prod smoke PASS on `29c77b7`. Pool 2/2 idle, max 2.

## Code (PR #159, no DDL)

- Facilities + pricing list singleflight shared-result fix: duplicate waiters
  never ran the closure, so they read a nil outer variable, cached nil, and
  returned empty lists under burst. Now uses the `Do` return value, nil-safe
  to `[]`. Availability already used the return value correctly.
- gzip pool to BestSpeed (level 1): ~half CPU on 200–2000 row JSON, output
  ~10–20% larger (still 4–8x), edge caches bytes.
- `LimitJSONBody` 1 MiB on project + vertical routers. Valid writes are KBs;
  huge bodies fail fast instead of OOMing free-plan memory. Chat keeps its own
  1 MiB cap (double-wrap harmless).

Proof: `internal/middleware` + `internal/features` unit green,
facilities + pricing integration green, build+vet+diff-check clean.

## DB (migration-only PR #158 → db-migrate apply → code)

- 098 (public) + centrum 0017 (project schema) `ANALYZE` only. No tables,
  policies, or grants touched. 098 first referenced `project_centrum` before
  it exists on fresh DBs (CI fresh-database failure); split fixes ordering.
- Fresh EXPLAIN (local, `project_centrum`, RLS context set), 2026-11-28:
  - facilities list → `Index Scan using sports_facilities_live_sort_idx`
  - pricing rule → `Index Scan Backward using
    sports_pricing_rules_tenant_id_valid_from_key` + heap filter
    (tiny table, correct)
  - per-facility bookings → `Index Scan using sports_bookings_no_overlap`
    (tenant+facility cond, date filter)
  - court blocks → `Index Scan using sports_court_blocks_tenant_date_idx`
  - `clients(token_hash)` → unique `clients_token_hash_key` exists; seq scan
    at 7 rows is the correct planner choice
  - `usage` month SUM → seq scan at 1 row; covering
    `usage_client_created_include_idx(client_id, created_at)` matches the
    predicate exactly at volume
- Verdict: no new index warranted (same as Batch 2, re-confirmed).

## Verified and deliberately not changed

- Staff lists already `LIMIT n+1` truncated, no `COUNT(*)`/`OFFSET`
  (bookings `truncated` flag). Keyset pagination not needed at this volume.
- Rule-cache singleflight across booking TXs skipped: sharing one TX's SELECT
  across snapshots weakens the price-resolution isolation for a rare
  same-date burst. 30s cache already absorbs it.
- Auth-path Redis pipelining / quota check-before-insert skipped: chat is
  provider-bound (seconds); ~1–2ms saved is noise for added coupling.
- RLS definer for public reads skipped: EXPLAIN shows index scans, RLS is
  initplan-hardened; definer adds review surface without measured >2ms win.
- Tenant L1 5s, Lua-merged gate, payments 4→3: remain declined (Batch 4 notes).
- Third TTL notch: needs product/freshness sign-off, not taken.
- Region affinity: `vercel.json` pins `sin1`; Supabase/Upstash regions not
  re-confirmed here — edge variance (50–100ms) still dominates origin
  (7–11ms). Residual, not a code defect.
