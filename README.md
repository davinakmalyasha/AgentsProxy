# AgentsProxy — one chi backend for all DFD frontends

One Go binary serves every frontend: client business sites (bookings, menus,
kantin), the DFD agency site + client portal, and machine agents over REST/MCP.

**Stack:** Go 1.27 + `chi/v5` + `pgx/v5/pgxpool` + `go-redis/v9` · **DB:** one
Supabase Postgres (shared schema + `tenant_id` + RLS) · **Cache:** Redis 8 ·
**Deploy:** Vercel / Railway / VPS.

Docs: `README.md` (this file = what it is) · `ARCHITECTURE.md` (how it works) ·
`docs/GOVERNANCE.md` (branch/DDL/test rules) · `RUNBOOK.md` (ops).

## Layout

```
cmd/server/            entrypoint (migrate → serve, :8080, chi)
internal/api/          root chi router: /healthz, /v1/*, /api/v1/*, /mcp
internal/features/     client runtime: registry + gate + vertical builder
  shared/              records, menu, booking, review, article, promo
internal/agency/       DFD agency backend (chi): content, intake, client,
                       admin, auth, agent, docs, misc, mcpserver
internal/agency/auth/  JWT cookies (humans) + SHA-256 bearer keys (machines)
tenants/<slug>/        per-client: backend/* (custom features), tools/,
                       memory/, skills/ (embedded via tenantembed.go)
internal/{config,db,cache,middleware,llm,auth,quota,notify,reminder,...}
internal/db/migrations/  single numbered chain (proxy 001-067 + agency 068+)
supabase/functions/    deliver-notification, deliver-reminders (serverless driver)
```

## Endpoints (all chi)

| Prefix | Who | Auth |
|---|---|---|
| `GET /healthz` | probes | none |
| `/v1/chat`, `/v1/agent/*` | client sites, widgets | `X-Client-Token` / public (rate-limited) |
| `/v1/auth/*` | staff login | email+password → `X-Session-Token` |
| `/v1/reports/*` | xlsx downloads | token-in-URL, 30-min TTL |
| `/v1/{vertical}/*` | client runtime (`fnb…sports`) | `X-Client-Token` + `RequireFeature` gate |
| `/v1/admin/*` | operator | `Bearer ADMIN_API_KEY` |
| `/api/v1/packages|projects|articles|promos|settings` | agency public site | none / IP rate-limit |
| `POST /api/v1/leads` | WhatsApp/custom inquiry | IP rate-limit |
| `POST /api/v1/orders` | signed-in web checkout | JWT cookie + idempotency key |
| `GET/POST /api/v1/auth/*` | agency humans | config/register/login/Google; JWT cookie for `/me`, `/logout` |
| `/api/v1/client/*` | client portal | cookie `CLIENT,SUPERADMIN,EDITOR` |
| `/api/v1/*` (admin) | back-office | cookie `SUPERADMIN` |
| `/api/v1/agent/*`, `/api/agent/*`, `/mcp` | machines | bearer key (`agent` scope) |

Errors: proxy shape `{"error":"..."}` on `/v1/*`; agency envelope
`{"success","message","data","error"}` on `/api/v1/*`. Both are chi handlers.

## Quick start (Laragon daily dev)

```bash
copy .env.example .env   # set DATABASE_URL, REDIS_URL, LLM_API_KEY, ADMIN_API_KEY, JWT_SECRET
go mod tidy
go run ./cmd/server      # migrations auto-run, :8080
node --env-file=.env scripts/seed-dev.mjs   # tenant "centrum", vertical "sports"
curl -s http://localhost:8080/healthz
go vet ./... && go test ./... -cover
```

## How one backend serves many frontends

One process, path + middleware isolation via chi groups (`internal/api/router.go`):
each group sets its own auth before its handlers, so a portal cookie can't
call proxy admin and a client token can't list agency leads — wrong group =
401 before any handler runs. Same Postgres, different tables/pools
(`pool` = `app_runtime` tenant writes, `adminPool` = cross-tenant/DDL).

* Static client sites (Hostinger) → `/v1/sports/bookings` + `X-Client-Token`.
* Agency Next.js (Vercel) → rewrite `/api/v1/*` → Go; portal `/api/v1/client/*`.
* Agents/MCP → `/api/v1/agent/*` + `/mcp` with bearer key.

## Add a feature

Shared (all tenants): copy `internal/features/shared/menu` → `.../<name>`,
set `Name/Kind/validator`, import in `cmd/server/main.go`.
Custom (one tenant): add a package under `tenants/<slug>/backend/<name>`
following the existing `centrum-badminton` features; isolation test required.
Agency: add handler under `internal/agency/<domain>/`, mount in
`internal/agency/router.go`. `go test ./...` + curl smoke.
