# AgentsProxy — one chi backend for all DFD frontends

One Go binary serves every frontend: client business sites (bookings, menus,
kantin), the DFD agency site + client portal, and machine agents over REST/MCP.

**Stack:** Go 1.27 + `chi/v5` + `pgx/v5/pgxpool` + `go-redis/v9` · **DB:** one
Supabase Postgres, one schema per project (`project_<slug>`) + `tenant_id` +
RLS in every project table · **Cache:** Redis 8 · **Deploy:** Vercel
(supported production host), with Docker/Compose kept as a production-like
local stack.

Docs: `README.md` (this file = what it is) · `ARCHITECTURE.md` (how it works) ·
`docs/GOVERNANCE.md` (branch/DDL/test rules) · `RUNBOOK.md` (ops) ·
`ops/RUNBOOK-staging.md` (free-plan staging).

## Live tenants

The client plane intentionally has one live tenant: `centrum` (Centrum
Badminton, vertical `sports`). The paused `dfd-agency` row in `clients` is
not a customer tenant; it exists only so its `frontend_origins` entry can
authorize CORS for the live DFD Agency site. See `RUNBOOK.md` §8a for the
prune script and tenant hygiene.

New projects are not shared vertical code: each one gets
`projects/<slug>/` and its own Postgres schema (`project_<slug>`). Centrum
now owns `project_centrum` and its frontend uses `/v1/p/centrum/...`.
The legacy `/v1/sports/...` alias is removed; a project tenant is never
served by `/v1/{vertical}`. New frontends use `/v1/p/<slug>/...` from day one.

## Layout

```
cmd/server/            entrypoint (:8080, chi; migrations are a deploy step)
internal/api/          root chi router: /healthz, /v1/*, /api/v1/*, /mcp
internal/features/     client runtime: registry + gate + vertical builder
  shared/              records, menu, booking, review, article, promo
internal/agency/       DFD agency backend (chi): content, intake, client,
                       admin, auth, agent, docs, misc, mcpserver
internal/agency/auth/  JWT cookies (humans) + SHA-256 bearer keys (machines)
projects/<slug>/       one client project: project.json, migrations/,
                       backend/ (HTTP features), agent/ (agent.md, skills,
                       mcp.json, tools) — generated imports in
                       internal/projectsgen/projects_gen.go
internal/{config,db,cache,middleware,llm,auth,quota,notify,reminder,...}
internal/db/migrations/  platform chain (proxy + agency + project schema support)
projectembed.go         embeds every project.json, agent config, and migrations
supabase/functions/    deliver-notification, deliver-reminders (serverless driver)
```

## Endpoints (all chi)

| Prefix | Who | Auth |
|---|---|---|
| `GET /healthz` | probes | none |
| `/v1/chat`, `/v1/agent/*` | client sites, widgets | `X-Client-Token` / public (rate-limited) |
| `/v1/auth/*` | staff login | email+password → `X-Session-Token` |
| `/v1/reports/*` | xlsx downloads | token-in-URL, 30-min TTL |
| `/v1/{vertical}/*` | platform tenants only | `X-Client-Token` + `RequireFeature` gate |
| `/v1/p/{slug}/*` | project-scoped runtime | `X-Client-Token` + project gate |
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
# 1. Start Laragon PostgreSQL + Redis (localhost only).
# 2. Create the local databases once:
createdb -h 127.0.0.1 -U postgres agents_proxy
createdb -h 127.0.0.1 -U postgres agents_proxy_test

# 3. Local-only environment. Never put production credentials here:
copy .env.example .env
copy .env.test.example .env.test

go mod tidy
make migrate                                # platform + project migrations (admin DSN)
make run                                    # :8080; RUN_MIGRATIONS=true is local-only
node --env-file=.env scripts/seed-dev.mjs   # tenant "centrum", vertical "sports"
curl -s http://localhost:8080/healthz
make test                                   # TEST_* from .env.test; never DATABASE_URL
make bench                                  # fresh bench DB + pprof/EXPLAIN evidence
```

`ENV=development` refuses any database host other than localhost. Staging is
the only environment allowed to reach a remote database, and only with
`ALLOW_REMOTE_DB=true`. The staging branch uses the staging Supabase project;
other previews carry no production secrets.

DB-backed tests read `TEST_DATABASE_URL`, `TEST_DATABASE_ADMIN_URL`, and
`TEST_REDIS_URL` only. `make test` requires all three to be loopback, and the
test helpers refuse a remote target even on a direct `go test` run. Each run
creates a fresh `agents_proxy_test_*` database and drops it on exit; Redis DB
15 is reserved for tests. `scripts/bench.sh` uses a separate
`agents_proxy_bench_*` database and Redis DB 14.

## How one backend serves many frontends

One process, path + middleware isolation via chi groups (`internal/api/router.go`):
each group sets its own auth before its handlers, so a portal cookie can't
call proxy admin and a client token can't list agency leads — wrong group =
401 before any handler runs. Same Postgres, different tables/pools
(`pool` = `app_runtime` tenant writes, `adminPool` = cross-tenant/DDL).

* Static and Next.js client sites → `/v1/p/<slug>/...` + `X-Client-Token`.
* Agency Next.js (Vercel) → rewrite `/api/v1/*` → Go; portal `/api/v1/client/*`.
* Agents/MCP → `/api/v1/agent/*` + `/mcp` with bearer key.

## Add a feature

Platform feature: add a package under `internal/features/shared/<name>`,
register it in `init()`, and blank-import it in
`internal/platformfeatures/platformfeatures.go` (the server and the
migration/provision commands all import that list).

Project feature: add `projects/<slug>/backend/<name>/` and call
`features.RegisterProject("<slug>", "<vertical>", ...)`; run
`go generate ./...` so the generated blank imports stay current. Project
features mount at `/v1/p/<slug>/...` and own their schema.

Agency: add a handler under `internal/agency/<domain>/` and mount it in
`internal/agency/router.go`. Run `make test` (or `go test -p 1 ./...`
against the local test targets) + curl smoke.

## License

Apache-2.0 � see [LICENSE](LICENSE). If you fork or reuse this code, keep the license file and copyright notice.
