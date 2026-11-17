# ARCHITECTURE — one chi backend, many frontends

## 1. Concept

One deployed Go binary serves two planes over one chi router:

* **Client plane** (`/v1/*`) — per-tenant runtime for client businesses.
  Every capability is a **feature** (`internal/features/`): core `records`
  (always on), shared (`menu/booking/review/article/promo`, gated per client),
  custom (`tenants/<slug>/backend/*`, same shape, isolation test required).
  Gate = in-memory check `name ∈ client.features ∧ AllowedForVertical`.
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
r.Mount("/v1", proxyRouter)     # chat, agent, auth, reports, verticals, admin
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

## 3. Data: one Postgres, two table families

* Client: `clients(token_hash,quota,status,vertical,features[])`, `usage`,
  `tenant_records(kind,payload)`, `sports_*` custom tables. Every child table:
  `tenant_id → clients(tenant_id) ON DELETE CASCADE` + `ENABLE+FORCE RLS` +
  policy `tenant_id = current_setting('app.tenant_id')` + `GRANT app_runtime`.
  Handlers run in `WithTenantTx` (sets the GUC per tx) — defense in depth.
* Agency: `users/leads/packages/orders/payments/projects/hosting/articles/
  contracts/order_milestones/client_requests/audit_logs/api_keys/
  agent_events/webhook_*` (migration 068). Admin-only via `adminPool`;
  no per-row tenant boundary (DFD is the only tenant).
* Rules: every query parameterized (`$1`); `service_role` never touches
  tenant tables; migrations additive-only, one chain, advisory-locked at boot
  (`RUN_MIGRATIONS=false` on Vercel — migrate as deploy step).
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

bcrypt passwords, `subtle.ConstantTimeCompare` on hashes, JWT ≥32 chars,
cookies `HttpOnly` (+`Secure/None` in prod), no token/secret in logs
(slog + Sentry redactors share one sensitive-key list).

## 5. Cache / delivery / ops

Redis is a hot layer, never source of truth: `ck:{hash}` 15m (evicted on
PATCH), `rl:{id}:{min}` 60s (fail-open when down), `quota:{id}:{month}` 30s.
Delivery is claim-based so both deploys work: Go tickers
(`BACKGROUND_TICKERS=true`, Railway/VPS) or `pg_cron` + Edge Functions
(`false`, Vercel) — atomic claim means exactly one driver sends each row.
Pool: runtime `Min 2/Max 20` (`2/0` on Vercel), admin `0/3` direct 5432,
`DB_USE_SIMPLE_PROTOCOL=true` on Supavisor 6543.
