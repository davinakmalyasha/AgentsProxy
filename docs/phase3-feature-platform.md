# Phase 3 — Feature Platform + Production Hardening (AgentsProxy)

> Executable build order for the other agent. Repo: `D:\MyProject\AgentsProxy`.
> Read `PRD.md`, `ARCHITECTURE.md`, `STANDARDS.md`, `DONE.md`, `RUNBOOK.md`, and all existing `.go` files first.
> **Locked stack:** Go 1.27 (Laragon `D:\laragon\bin\go\go1.27.0\bin\go.exe`), chi v5, pgx v5, go-redis v9, slog, real Laragon Postgres + real Redis 8.10 for tests. No Docker for dev. Do NOT touch Langchain or DFDWebsite.

---

## 0. Environment (critical, Windows PowerShell)

```powershell
# ALWAYS load .env first, ALWAYS use the Laragon Go binary directly
. .\scripts\load-env.ps1
& "D:\laragon\bin\go\go1.27.0\bin\go.exe" vet ./...
& "D:\laragon\bin\go\go1.27.0\bin\go.exe" build ./...
& "D:\laragon\bin\go\go1.27.0\bin\go.exe" test ./... -cover
```
`go.mod` requires `go 1.27.0`; the on-PATH `go` is 1.26.5 and will fail with a toolchain mismatch — never rely on `go` from PATH.

---

## 1. Data model — migrations

### `internal/db/migrations/004_features.sql`
- `ALTER TABLE clients ADD COLUMN IF NOT EXISTS vertical text NOT NULL DEFAULT '';`
- `ALTER TABLE clients ADD COLUMN IF NOT EXISTS features text[] NOT NULL DEFAULT '{}';`
- `CREATE INDEX IF NOT EXISTS clients_vertical_idx ON clients(vertical);`
- Backfill dev row (idempotent): `UPDATE clients SET vertical = 'fnb', features = ARRAY['records','menu','booking','review','article','promo'] WHERE tenant_id = 'dev' AND features = '{}';`
- Keep everything idempotent (`IF NOT EXISTS`), consistent with existing migrations.

### `internal/db/migrations/005_rbac.sql`
- Create runtime role (idempotent DO block, mirroring the existing `app` role bootstrap in tests):
  - `CREATE ROLE app_runtime LOGIN;` (or `ALTER ROLE ... LOGIN` if exists)
  - `GRANT USAGE ON SCHEMA public TO app_runtime;`
  - `GRANT SELECT ON clients TO app_runtime;`  ← read-only on clients
  - `GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_records TO app_runtime;`
  - `GRANT SELECT, INSERT, UPDATE, DELETE ON usage TO app_runtime;`
  - `GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_runtime;`
- Keep an admin-capable role for DDL/migrations (the existing admin DSN role is fine; document it).
- Ensure runtime role has NO DML on `clients` (only SELECT) and NO DDL anywhere.

### `internal/db/migrations/006_*.sql`
- Feature indices as the first shared features require — decided during build (e.g. `CREATE INDEX ... ON tenant_records ((payload->>'date'))` for booking). Add one migration per feature that needs an indexed search field.

---

## 2. Feature engine — `internal/features/`

```
internal/features/
  registry.go    Feature struct + Register() + lookup + boot-time duplicate-route panic
  defaults.go    vertical → default feature set map (single source of truth)
  gate.go        RequireFeature middleware (checks cached client's features[] + vertical allow-list)
  builder.go     Per-vertical router assembly from the registry
  shared/        records (moved from api/business.go), menu, booking, review, article, promo
  clients/_template/   copy-paste starter for a custom feature
```

### `registry.go`
```go
type Feature struct {
    Name            string                          // unique, e.g. "menu"
    VerticalSlugs   []string                        // empty = all verticals
    Routes          func(chi.Router)                // nil = no custom routes
    ValidatePayload func(json.RawMessage) error     // nil = no validation
    DefaultOn       bool                            // included in DefaultSetFor(vertical)?
}
```
- `Register(f Feature)` at boot; **panic on duplicate feature name or duplicate route pattern** (misconfig dies at startup, not in prod).
- Expose `All() []Feature`, `Get(name) (Feature, bool)`, `Names() []string`, `AllowedForVertical(name, vertical) bool`.

### `defaults.go`
- `map[string][]string` — e.g. `"fnb" → {"records","menu","booking","review","article","promo"}`. Same defaults for all 12 verticals initially (override per vertical if any feature is vertical-specific).
- `DefaultSetFor(vertical string) []string`.

### `gate.go`
- `RequireFeature(name string) func(http.Handler) http.Handler` — reads the client model from context (set by `withClientAuth`), checks:
  1. client is active (already done by auth)
  2. `name` ∈ client's `features` array (from the cached `ClientModel`)
  3. `AllowedForVertical(name, client.vertical)`
  - Any miss → `403 {"error":"feature not enabled for this client"}`.
  - Zero extra DB hit — reads the same cached client model the auth middleware already resolved.

### `builder.go`
- `func BuildVerticalRouter(b *api.Business, vertical string, features []Feature, gate ...) http.Handler`
- For each vertical: client-auth → `records` routes (feature name `records`, default-on) → each registered feature's subrouter wrapped in `RequireFeature(feature.Name)`.
- Mount in `internal/api/router.go` — one route per vertical from `allowedVerticals` (12 lines), each delegating to the builder. Keep `/v1/chat`, `/v1/admin/*`, `/healthz` as-is.
- New URL shape: `/v1/{vertical}/{feature}/...` (e.g. `/v1/fnb/menu/items`, `/v1/klinik/bookings`). The existing `/v1/{vertical}/records` stays as the `records` feature.

---

## 3. Refactor `internal/api/business.go` → `records` shared feature

- Move the generic CRUD engine into `internal/features/shared/records/`:
  - Keep `withTenantTx` (SET LOCAL `app.tenant_id` per TX — never `pool.Exec`).
  - Keep client-auth middleware (or move to a shared place and reuse).
  - Keep `allowedVerticals` (single source for the 12 strings; the feature registry + builder consume it).
- **Fix soft-delete leak (hardening #1):** list and get-one default to `status != 'deleted'`; add optional `?status=` query filter.
- **Cursor pagination (hardening #2):** replace `LIMIT/OFFSET` with cursor-based:
  - Request: `?before=<cursor>` (base64 of `created_at|id`) or `?limit=N` (default 20, max 100).
  - Response: `{items, next_cursor, has_more}`. `total` removed from client lists (keep COUNT only in admin usage views).
  - Index-friendly: `WHERE created_at < $x OR (created_at = $x AND id < $y)` ordering by `created_at DESC, id DESC`.
- Update all affected tests (`business_test.go`) to the new list shape; keep `TestBusiness_TenantIsolation` and `TestBusiness_RLSEnforcedAtDB` — they must still pass.

---

## 4. Shared features (6)

Each feature = a small package under `internal/features/shared/` with: allowed verticals (all 12 unless noted), `ValidatePayload`, routes (nil unless behavior needed), and tests.

| Feature | Kind value | Payload validation (non-exhaustive) | Notes |
|---|---|---|---|
| `records` | any | none beyond existing | the generic engine; moved, not new |
| `menu` | `menu_item` | `name` non-empty, `price` number ≥ 0 | validates into existing `tenant_records` |
| `booking` | `booking` | `date`/`datetime` required, `name` non-empty | payload `date` indexed in `006` |
| `review` | `review` | `rating` 1–5, `author` non-empty | |
| `article` | `article` | `title` non-empty, `body` non-empty | |
| `promo` | `promo` | `title` non-empty, `discount` ≥ 0 | |

- All write to `tenant_records` through `withTenantTx` only.
- `kind` is set server-side from the feature name — clients cannot fake a kind.
- Tests per feature: happy create, validation 400, tenant isolation (tenant B cannot list/read A's rows via that feature's routes).

---

## 5. Production hardening (all 8, this pass)

| # | Upgrade | Where |
|---|---|---|
| 1 | Soft-delete leak fix | `records` list/get (above) |
| 2 | Cursor pagination | `records` list (above) |
| 3 | Two-role split | `005_rbac.sql` + pool config (below) |
| 4 | Supabase pooler compatibility | runtime pool → Supavisor 6543 tx-mode + simple protocol (`QueryExecModeSimpleProtocol`); admin/migrations → 5432 direct; `DB_POOL_MIN/DB_POOL_MAX` env (defaults 2/20) |
| 5 | Rate limit by client ID | refactor `middleware/ratelimit.go` — resolve auth first, key = `c:{client_id}`; IP fallback only pre-auth |
| 6 | Shared-DB tenant rule | ARCHITECTURE.md + RUNBOOK.md: DFDWebsite/Edge Functions must set `app.tenant_id`; `service_role` banned on tenant tables |
| 7 | Custom-feature table checklist | `clients/_template/` enforces: custom tables ship their own migration with RLS + tenant_id + isolation test |
| 8 | Redis degradation matrix | documented in ARCHITECTURE.md: cache→DB fallback, quota→DB SUM, rate-limit fail-open, feature toggle ≤15min (instant via admin PATCH evict), quota = soft cap |

---

## 6. API changes

### `POST /v1/admin/clients`
- Accept optional `vertical` and `features`.
- When omitted: `vertical` defaults to `'fnb'`, `features` = `DefaultSetFor(vertical)`.
- Validate `features` against the registry (unknown name → 400).
- Response includes `vertical` + `features`.

### `PATCH /v1/admin/clients/{id}`
- Accept `features` (array) — validates names against registry, evicts cache (existing behavior).
- Response includes updated `vertical` + `features`.

### `GET /v1/admin/clients`
- New: list clients with `id, name, tenant_id, vertical, features, status, monthly_quota_tokens`.
- Use the admin pool (full DML on `clients`), not the runtime pool.

### Cache model (`internal/cache/client.go`)
- `ClientModel` += `Vertical string` and `Features []string`; update SELECT to include both columns; keep TTL 15m.

---

## 7. `internal/features/clients/_template/` (custom-feature starter)

- Copy-paste starter containing:
  - `feature.go` — registers a `Feature` with `VerticalSlugs` + tenant restriction, `Routes`, `ValidatePayload`
  - `feature_test.go` — **mandatory isolation test**: tenant B gets 403/404 on tenant A's custom endpoint; validation 400s; happy path
  - `README.md` — `Status: live | promoted | archived`, who owns it, why it exists, which tenant(s)
- Every custom feature folder = one folder per client slug, `tenant_id` constant inside.

---

## 8. Docs (update all)

- **PRD.md** — mark §6.2 as "records feature", add §6.5 Feature platform, §9 cursor pagination, §13 extensibility = "one folder + one registry line".
- **ARCHITECTURE.md** — new "Feature platform" section (registry/gate/builder, tiers, promotion rule, custom checklist, escape hatch), shared-DB tenant rule, Redis degradation matrix, secrets policy, pooler/simple-protocol config.
- **RUNBOOK.md** — env table += `DB_POOL_MIN/MAX`, runtime vs admin DSNs, Supabase 6543/5432, feature toggle procedure, escape-hatch decision rule ("in-request if <300ms and no secrets; else Edge Function").
- **README.md** — endpoints table += `GET /v1/admin/clients`, feature URLs, "how to add a feature / custom feature".
- **DONE.md** — reflect Phase 3: new endpoints, migrations 004–006, feature engine, all 8 hardening items ✅/🟡 with notes.

---

## 9. Tests + verification (gate for done)

- `go vet ./...` clean, `go build ./...` clean.
- `go test ./... -cover` — ALL existing tests updated and passing; **every package ≥70%** (new feature packages included); integration tests run against real Laragon Postgres + Redis (env-loaded `.env`).
- Required new tests:
  - Registry: defaults per vertical, `AllowedForVertical`, duplicate-name panic, duplicate-route panic.
  - Gate: feature off → 403; vertical not allowed → 403; default set correct.
  - Records: soft-delete excluded from list by default; `?status=deleted` includes it; cursor pagination (`next_cursor`, `has_more`, stable ordering).
  - Each shared feature: validation 400 + tenant isolation.
  - Admin: create with vertical/features, PATCH features toggles gate (immediate), GET list.
  - Runtime role: prove runtime pool (as `app_runtime`) cannot INSERT/UPDATE/DELETE on `clients` (SQLSTATE 42501).
- Run the full suite at the end and paste the coverage table into your final report.

---

## 10. Execution order (do NOT skip steps)

1. `004_features.sql` + cache `ClientModel` + admin endpoints (create with vertical/features, PATCH features, GET list)
2. `features/` registry + defaults + gate + builder + tests
3. Refactor `business.go` → `records` shared feature; soft-delete + cursor pagination
4. Shared features: menu, booking, review, article, promo (+ `006` index migration as needed)
5. `005_rbac.sql` role split + pool/simple-protocol config + rate-limit refactor
6. `clients/_template/` + escape-hatch docs
7. Docs (PRD, ARCHITECTURE, RUNBOOK, README, DONE.md)
8. Final gates: vet, build, `go test ./... -cover` green, coverage table in report

---

## 11. Final report format (return this to the user)

- What shipped (features, endpoints, migrations, hardening items, each with file paths)
- Test results: full coverage table + `go vet/build` green confirmation
- Any deviations from this spec + why
- Known limitations carried forward
- The exact commands used to verify
