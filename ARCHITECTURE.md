# ARCHITECTURE — one chi backend, many frontends

## 1. Concept

One deployed Go binary serves two planes over one chi router:

* **Client plane** (`/v1/*`) — per-project runtime for client businesses.
  Every **business capability** is a feature (`internal/features/`): core
  `records` (always on), platform features (`menu/booking/review/article/promo`,
  gated per client), and project features (`projects/<slug>/backend/*`, mounted
  at `/v1/p/<slug>/*`, isolation test required). Shared services (`/v1/chat`,
  `/v1/agent`, `/v1/auth`, `/v1/reports`, `/v1/push`) are not features; they
  are tenant-scoped platform services and keep their own auth gates.
  Gate = in-memory check `name ∈ client.features ∧ AllowedForVertical`; a
  project gate also pins the tenant to the project named in the URL.
* **Agency plane** (`/api/v1/*`, `/mcp`) — DFD's own backend: public catalog,
  intake (leads/orders), client portal, back-office admin, machine agent API.
  Single-tenant tables (`users/leads/orders/projects/...`), no RLS needed —
  access is cookie/bearer role-checked before any query.

Both planes share Postgres + Redis + config + middleware, but never each
other's tables. A bug in invoice PDF gen can't break a court booking.

## 2. Router: chi everywhere

`internal/api/router.go` builds one `chi.NewRouter()`:

```
r.Use(Recovery, SecurityHeaders, RequestID, Logging,
      StripTrailingSlash, CORSResolving, Gzip)
r.Get("/healthz", ...)
r.Mount("/v1", proxyRouter)     # chat, agent, auth, reports, verticals, /p/<slug>, admin
r.Mount("/api/v1", agencyRouter) # internal/agency/router.go (chi)
r.Mount("/mcp", mcpHandler)
```

Why chi, not stdlib `ServeMux`: the proxy has nested groups with scoped
middleware (`/v1` rate-limit → `/v1/admin` admin-auth + different pool →
`/{vertical}/{feature}` built at runtime from the feature registry). Stdlib
handles flat lists well (the agency CRUD was flat) but needs sub-muxes +
`StripPrefix` shims for the rest; chi is `http.Handler`-compatible so flat
groups mount as leaves. One router pattern, zero `NewServeMux` left.

Rules: identity middleware before rate-limit (limiter buckets on identity);
`r.PathValue` works under chi (chi sets path values); method + path exact.

## 3. Data: one Postgres, platform + project schemas

* Client platform: `clients(token_hash,quota,status,vertical,features[],
  db_schema)`, `usage`, and the shared identity tables. `clients.db_schema`
  routes a tenant to its business schema.
* Project business data: one Postgres schema per project, e.g.
  `project_centrum`/`project_acme`. Centrum's tables were moved out of
  `public` by `projects/centrum/migrations/0001_move_to_project_schema.sql`;
  every project, including Centrum, now sets `"schema": "project_<slug>"` in
  project.json and puts its tables in `projects/<slug>/migrations/`. Every
  table still carries `tenant_id → clients(tenant_id) ON DELETE CASCADE` +
  `ENABLE+FORCE RLS` + policy `tenant_id = current_setting('app.tenant_id')`
  + `GRANT app_runtime`. Handlers run in `WithTenantTx`, which sets both the
  GUC and `search_path` from `clients.db_schema` per transaction. Every
  records/feature query also carries an explicit `tenant_id` predicate.
  RLS is defense-in-depth, not the sole boundary: `app_runtime` can set the
  tenant GUC and the table owner/definers bypass it. The route gate pins the
  project URL to its derived schema, `WithTx` fails closed for an unknown
  tenant, and the usage insert is bound to `app.client_id` (086). Project
  tenants are reachable only under `/v1/p/<slug>/...`.
  `/v1/{vertical}` is reserved for platform tenants in `public`; a project
  tenant never creates a vertical alias. Centrum's frontend uses
  `/v1/p/centrum/...`, and the old `/v1/sports/...` surface was removed after
  its stability window. The data boundary is project-scoped.
  The database enforces the namespace: `clients.db_schema` is `public` (the
  legacy platform row only) or `project_*`, and a partial unique index allows
  exactly one client per project schema. A `project_*` client requires a
  `projects/<slug>/project.json` folder and a complete project migration
  ledger; a platform tenant uses `public` and needs no folder. The admin API
  creates rows, not DDL: `migrate apply` creates the schema and tables before
  the client exists. Admin provisioning turns a unique violation into a 409;
  `migrate check` also requires the index, reconciles the client's vertical
  and feature entitlement with `project.json`, and reports historical drift.
  `project_migrations` records the schema each version ran in, so a project
  that changes its schema fails loudly instead of silently skipping every
  applied migration. `migrate check` also audits ENABLE+FORCE RLS on every
  table in every project schema.
  Staff sessions are platform auth state in `public.staff_sessions`; staff
  users remain project data in each schema. `internal/authapi` owns the session
  directory, token primitives, and the `/v1/auth` route shell, but knows no
  project table. A project registers an `authapi.Provider`; Centrum's lives in
  `projects/centrum/backend/auth` and reads `sports_users` inside
  `tenantdb.WithActiveTx`, so RLS and the project schema apply exactly as they
  do for every other feature. Adding staff auth to a project is a provider
  package, not a platform function edit.
  Login is throttled per tenant/email in Redis (default 10 attempts/min, then
  5 failed password checks lock the account for 15 minutes); the key is a
  SHA-256 of tenant+email, unknown users still pay a bcrypt comparison, and a
  Redis outage fails open so an operator is not locked out by the cache.
  The `/v1/admin` surface writes an append-only `public.admin_audit_log` row
  before each handler runs; the actor is an `ADMIN_API_KEY` fingerprint and
  request bodies are never stored. Report capabilities
  (`/v1/reports/{token}`, `/v1/reports/jobs/{id}`) are redacted from
  application logs, Sentry breadcrumbs, and panic reports. Outbound Web Push
  endpoints are restricted to the browser push services in
  `internal/pushsafe`, both on subscribe and immediately before each send.
  Production runs PostgreSQL 17 (the CI and compose images match).
* Agency: `users/leads/packages/orders/payments/projects/hosting/articles/
  contracts/order_milestones/client_requests/audit_logs/api_keys/
  agent_events/webhook_*` (migration 068). Admin-only via `adminPool`;
  no per-row tenant boundary (DFD is the only tenant). `app_runtime` is
  deliberately not granted agency DML (migration 082).
* ADK conversations: `sessions/events/app_states/user_states` set
  `app.tenant_id` in every session-service transaction (085). Their RLS
  policies require `app_name = current_setting('app.tenant_id', true)`, so a
  missing predicate reads zero rows instead of every tenant's transcript.
  The admin pool still bypasses RLS for maintenance.
* Rules: every query parameterized (`$1`); `service_role` never touches
  tenant tables; migrations are checksummed and advisory-locked. They follow
  expand/contract: additive changes first, destructive cleanup in a later
  contract migration after the stability window. `RUN_MIGRATIONS=false` on
  Vercel — migrate as a deploy step.
  Platform migrations live in `internal/db/migrations/`; project migrations
  live in `projects/<slug>/migrations/` and are recorded in
  `public.project_migrations` with a per-project version scope.
* Double-booking is DB-enforced: `EXCLUDE USING gist
  (tenant_id, facility_id, tsrange(date+start,date+end))`; `23P01` → 409.

## 4. Auth

| Credential | Who | Scope |
|---|---|---|
| `X-Client-Token` | a tenant client | full access to that tenant's features |
| `X-Session-Token` | staff (`owner/pegawai`) | role-limited per request |
| `Bearer ADMIN_API_KEY` | operator | provision + cross-tenant (server-side only) |
| JWT cookie (`token`) | agency human | `SUPERADMIN/EDITOR/CLIENT` roles, tokenVersion revocation |
| Bearer agency key | machine agent | SHA-256 hash, `scopes[]` (`agent`) |

Agency auth is one identity model: email/username + password (SUPERADMIN adds
a 6-digit Resend email code), Google one-click for CLIENT only, and
email-verified CLIENT self-signup (`signup_requests` → user on code verify).
Sessions are 30-day JWTs (`JWT_TTL_HOURS=720`) refreshed while active under
7 days; `tokenVersion` revokes every session on logout/password change.
Google never links a staff account. Web orders require a session and bind
`orders.user_id`; the legacy `users.lead_id` link still grants portal access.

bcrypt passwords, `subtle.ConstantTimeCompare` on hashes, JWT ≥32 chars,
cookies `HttpOnly` + `SameSite=Strict` (`Secure` in prod), no token/secret in logs
(slog + Sentry redactors share one sensitive-key list).

## 5. Cache / delivery / ops

Redis is a hot layer, never source of truth: `ck:{hash}` 15m (evicted on
PATCH), `rl:{id}:{min}` 60s (fail-open when down), `quota:{id}:{month}` 30s.
Delivery is claim-based so both deploys work: Go tickers
(`BACKGROUND_TICKERS=true`, Railway/VPS) or `pg_cron` + Edge Functions
(`false`, Vercel) — atomic claim means exactly one driver sends each row.
Pool: runtime `Min 2/Max 20` (`Min 0/Max 2` on Vercel), admin `0/3` direct 5432,
`DB_USE_SIMPLE_PROTOCOL=true` on Supavisor 6543.
