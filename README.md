# AgentsProxy — DFD Single Go Monolith (AI + Business)

> One deployed binary for all DFD agentic clients. `POST /v1/chat` (quota-metered LLM proxy)
> + `POST/GET /v1/{vertical}/*` (tenant CRUD, Supabase Postgres RLS) + `POST /v1/admin/*`
> (create token+quota). Client sites are static (Hostinger) and call this.

**Stack:** Go **1.27** + `chi/v5` + `pgx/v5/pgxpool` + `go-redis/v9` · **DB:** one Supabase
Postgres 17 shared by all tenants (RLS) · **Cache:** Redis 8 · **Deploy:** Vercel (Go runtime)
or Railway/VPS.

**Docs:** this file = *what it is* · [`ARCHITECTURE.md`](ARCHITECTURE.md) = *how it's built and
why* · [`docs/GOVERNANCE.md`](docs/GOVERNANCE.md) = *development rules (branches, CI, envs,
testing, tenants — read before opening a PR)* · [`docs/refinementNotes.md`](docs/refinementNotes.md) = *what to fix next*.

---

## 1. Why this service exists

### 1.1 The position in the triangle

```
LangChain (D:\MyProject\Langchain) — FACTORY, private, your laptop + phone via Tailscale
  DFD workspace _dfd + dfd_bridge (milestones) + proxy_bridge (create token) — private, scopes=["dfd"] only
        | create-client-token (ADMIN_API_KEY)          | milestones/notes (DFD_API_KEY)
        v                                              v
Go Proxy (this repo, deployed)                   DFD Backend (D:\MyProject\DFDWebsite/server, Express 5 on Railway + MySQL Aiven)
  POST /v1/chat per-client token                      Prisma + shared schemas + WhatsApp
        \                                               ▲ same Supabase Postgres (shared DB, RLS)
         \ ⑤ chat widget calls (NEXT_PUBLIC_AGENTS_PROXY_URL + per-client token)
          v
Client Sites (Hostinger Unlimited static, 12+ verticals from D:\MyProject\MainTemplates)
```

- **Agents are on top (factory).** Proxies are in the middle (runtime, deployed, always-on).
  Client sites are at the bottom (Hostinger static, no per-client server).
- "Unified" means **one deployed instance serves all clients** — not one instance per client.
- This repo does **not** build `MainTemplates`, and does **not** manage DFD milestones. Those
  belong to other agents/tools.

### 1.2 The business reason

Every agentic client subscription includes "unlimited agentic possibility," folded into the
Rp1.5M build renewal. **The client must never hold the master LLM key.** They hold a per-client
token; you hold the master key on the server. This service exists to:

1. **Hold THE DeepSeek/mimo key** (`LLM_API_KEY`) — never in a repo, never in a client site.
2. **Meter per-client usage** — monthly quota plus a sliding-window rate limit.
3. **Log everything** for billing and dashboards — the DFD backend reads the same Postgres.

### 1.3 Why Go, and why only here

The DFD backend stays Node (Express 5 + Prisma + `whatsapp-web.js`). Rewriting it in Go is
weeks of risk for zero user gain. This gateway is the one place Go genuinely wins: a single
static binary (~15 MB), `log/slog` JSON, one process that serves every vertical, and the
runtime's current GC/allocator work as free performance.

---

## 2. Goals and non-goals

### Goals

1. `go build ./...`, `go vet ./...`, `go test ./...` all clean against real Postgres + Redis.
2. Per-client monthly quota enforced server-side; pay-as-you-go AI; host/domain renewal model.
3. Hostinger holds **all** client statics — zero per-client infrastructure, one Postgres for
   all data.
4. **Adding vertical 13+ is one string.** Adding a per-tenant feature is one folder
   (see *How to Add a Feature*).

### Non-goals

- Building `MainTemplates`, DFD milestones, or orders.
- A database, server, or Redis keyspace per client — one shared schema with `tenant_id` + RLS.
- ~~No Vercel deploy~~ — **superseded.** Vercel's 300s function ceiling comfortably covers the
  60s `llm.Client` cap, and `vercel.json` pins `maxDuration: 300`. Vercel, Railway, and VPS
  are all supported; `BACKGROUND_TICKERS` selects the delivery driver.

---

## 3. Quick start (Laragon — daily dev, not Docker)

1. Copy env:
   ```bash
   copy .env.example .env
   # Edit: DATABASE_URL (postgres://postgres@localhost:5432/agents_proxy?sslmode=disable for Laragon)
   #       REDIS_URL=localhost:6379
   #       LLM_API_KEY=sk-... (DeepSeek/mimo), LLM_BASE_URL=https://api.deepseek.com
   #       ADMIN_API_KEY=$(openssl rand -hex 32) — generate once, same value in your Langchain dfd_bridge
   ```
2. Laragon → **Start All** → `Postgres` (5432) + `Redis` (6379) green (Services). Create DB
   `agents_proxy` in HeidiSQL if first run.
3. Run (from `D:\MyProject\AgentsProxy`):
   ```bash
   go mod tidy
   go run ./cmd/server
   # Migrations auto-run on boot. Server on :8080.
   ```
   > If the toolchain complains, the `go` on `PATH` may be older than `go.mod` requires. Use
   > `D:\laragon\bin\go\go1.27.0\bin\go.exe` explicitly.
4. Provision a tenant (no migration seeds one — `003` was retired because it published a
   credential on every environment, including production):
   ```bash
   node --env-file=.env scripts/seed-dev.mjs
   # defaults to tenant "centrum", vertical "sports"
   ```
   Until this runs, `clients` is empty and every public read
   (`/v1/sports/{availability,facilities,pricing}`) answers `400 {"error":"unknown tenant"}`.
5. Verify:
   ```bash
   curl -s http://localhost:8080/healthz
   # {"status":"ok","version":"dev","db":true,"redis":true}
   curl -s "http://localhost:8080/v1/sports/availability/?date=$(date +%F)&tenant_id=centrum&type=badminton"
   go vet ./... && go test ./... -cover
   ```
6. HeidiSQL → `SELECT * FROM clients;`. `redis-cli KEYS "ck:*" "rl:*" "quota:*"`.

> **Prod only** — `docker compose up --build` (postgres + redis + app) is for Railway/VPS.

### Quality gates

```bash
go mod tidy && go vet ./... && go fmt ./... && go test ./... -cover && go build ./...
```

CI additionally enforces `gofmt` cleanliness and a `go.mod`/`go.sum` tidy diff. Note there is
**no coverage threshold enforced** today and no `golangci-lint` config — both are tracked as
Tier 1 items in [`docs/refinementNotes.md`](docs/refinementNotes.md) §2 (C2, C3).

---

## 4. Environment

See `.env.example` for the annotated list. Required: `DATABASE_URL`, `REDIS_URL`,
`LLM_API_KEY`, `ADMIN_API_KEY`.

| Var | Default | Notes |
|---|---|---|
| `PORT` | `8080` | |
| `DATABASE_URL` | — (required) | Laragon local: `postgres://postgres@localhost:5432/agents_proxy?sslmode=disable` |
| `DATABASE_ADMIN_URL` | — | Superuser DSN. Migrations + admin routes **only**. Never the runtime pool. |
| `REDIS_URL` | `localhost:6379` | |
| `LLM_BASE_URL` | `https://api.deepseek.com` | Provider-swappable |
| `LLM_API_KEY` | — (required) | Secret |
| `LLM_MODEL` | `deepseek-chat` | |
| `ADMIN_API_KEY` | — (required) | `Authorization: Bearer $ADMIN_API_KEY` |
| `MAX_TOKENS_PER_REQUEST` | `2048` | |
| `RATE_LIMIT_PER_MINUTE` | `180` | ⚠️ `.env.example` still says `60` — see refinementNotes C4 |
| `AGENT_RATE_LIMIT_PER_MINUTE` | — | Separate bucket so landing-widget polls don't eat chat quota |
| `BACKGROUND_TICKERS` | `true` | `false` on Vercel — pg_cron owns the clock instead |
| `TRUSTED_PROXY_CIDRS` | — | ⚠️ **Required on Vercel** or every client buckets as one IP |
| `ALLOW_ORIGINS` | `*` in dev | Prod: `https://a.my.id,https://b.my.id`. `ENV=production` tightens CORS. |
| `SENTRY_*` | — | `DSN`, `ENV`, `RELEASE`, `TRACES_SAMPLE_RATE` — all optional |

> **Three vars the code reads but `.env.example` omits** — `DB_POOL_MIN` (default 2),
> `DB_POOL_MAX` (default 20), `DB_USE_SIMPLE_PROTOCOL` (default false, **must be `true` for
> Supavisor port 6543**). Vercel needs `DB_POOL_MAX=2` and `DB_POOL_MIN=0`, because the
> default is *per instance* and 20 lambdas would open 400 connections. See
> `ops/RUNBOOK-vercel.md`.

---

## 5. Endpoints

| Method | Path | Auth | Body / Query |
|---|---|---|---|
| `GET` | `/healthz` | none | — |
| `POST` | `/v1/chat` | `client_token` in body | `{"client_token":"...","messages":[{"role":"user","content":"..."}],"model":"..."}` |
| `POST` | `/v1/agent/client-chat` | none (public widget) | Rate-limited separately; namespaced memory |
| `POST` | `/v1/agent/chat` | `X-Session-Token` | Staff/owner chat with tools |
| `DELETE` | `/v1/agent/history` | `X-Client-Token` | Client-only memory wipe |
| `GET` | `/v1/reports/{token}` | the token *is* the auth | xlsx download, tenant-scoped, 30-min TTL |
| `POST` | `/v1/auth/login` `·` `logout` · `POST register` · `GET me` | — / `X-Session-Token` | Staff login (bcrypt, DB sessions) |
| `POST` | `/v1/{vertical}/records` | `X-Client-Token` | `{kind, title, payload}` |
| `GET` | `/v1/{vertical}/records?kind=&q=&before=&limit=&status=` | `X-Client-Token` | Cursor pagination; `status=active` default, `status=deleted` for tombstones |
| `GET` | `/v1/{vertical}/records/{id}` | `X-Client-Token` | — |
| `PATCH` | `/v1/{vertical}/records/{id}` | `X-Client-Token` | `{title?, payload?, status?}` |
| `DELETE` | `/v1/{vertical}/records/{id}` | `X-Client-Token` | — (soft; `status=deleted`) |
| `POST/GET/PATCH/DELETE` | `/v1/{vertical}/{feature}[/{id}]` | `X-Client-Token` | Generic adapter for records-backed features |
| `POST` | `/v1/admin/clients` | `Bearer ADMIN_API_KEY` | `{name, tenant_id, monthly_quota_tokens?, vertical?, features?}` — token returned **once** |
| `GET` | `/v1/admin/clients` | `Bearer ADMIN_API_KEY` | — |
| `PATCH` | `/v1/admin/clients/{id}` | `Bearer ADMIN_API_KEY` | `{quota?, status?, vertical?, features?}` (cache evicted) |
| `GET` | `/v1/admin/usage` | `Bearer ADMIN_API_KEY` | `?from=&to=&client=` |

Allowed `vertical`: `fnb, salon, laundry, klinik, gym, bimbel, bengkel, beauty, travel,
property, katering, photo, sports`.
Shared `feature`s: `records` (always on), `menu`, `booking`, `review`, `article`, `promo`.
Custom features live at `tenants/<slug>/backend/<name>/`.

Errors: `400 {"error":"..."}`, `401`/`403` for auth, `429 {"error":"quota exceeded",
"resetAt":"<ISO>"}` + `Retry-After`, `500 {"error":"internal"}` — provider errors are never
echoed verbatim.

> **There is no OpenAPI spec yet.** The contract is maintained as this table plus
> `ARCHITECTURE.md` §14, and has already drifted from `.env.example` once (see
> refinementNotes C4). Machine-readable spec + codegen is a Tier 1 item.

---

## 6. Sports vertical (the pilot)

```
POST   /v1/sports/facilities                      court/field/pool (name, type, capacity, price_per_hour, open/close)
GET    /v1/sports/facilities                      list
POST   /v1/sports/bookings                        book a slot — 409 "slot already booked" on overlap, DB-enforced
GET    /v1/sports/bookings?date=YYYY-MM-DD        daily schedule
POST   /v1/sports/inventory/{id}/adjust           {delta} — restock (+N) / sale (−N), never negative
POST   /v1/sports/members/{id}/topup              {amount} — prepaid balance
POST   /v1/sports/payments                        record a cash/QRIS payment; payment_status auto-recalcs
GET    /v1/sports/payments?booking_id=            payments for a booking
POST   /v1/sports/pricing · GET                   date-aware pricing rule (day/evening rates, open/close)
POST   /v1/sports/expenses                        {category, note}
POST   /v1/sports/customers                       customer directory (no-show count, visit aggregation)
POST   /v1/sports/courtblocks                     block a court / all courts (maintenance, private events)
POST   /v1/sports/series                          recurring weekly booking (community groups)
POST   /v1/sports/kantin/products                 canteen product (cost + stock + low-stock threshold)
POST   /v1/sports/kantin/sales                    ATOMIC sale — total server-computed, stock decremented, movements logged
GET    /v1/sports/availability?date=              public landing-page availability
```

The double-booking guarantee lives at the **database** level: `sports_bookings` has an
exclusion constraint over `(tenant_id, facility_id, tsrange(date+start, date+end))`, so no
application bug can overlap a court. See `ARCHITECTURE.md` §11.3.

Bookings support the full lifecycle (`pending → confirmed → checked_in → completed / cancelled /
no_show`), `source` (whatsapp/walk_in), `override_price`, and `payment_status`. A
cancelled/no-show slot is rebookable.

A sports client needs features `facilities`, `bookings`, `inventory`, `members` — enable via
`PATCH /v1/admin/clients/{id}`.

---

## 8. Staff login

```
POST /v1/auth/login   {"tenant_id":"centrum","email":"owner@...","password":"..."}
  → {"token":"<session>", "user":{...}}     # send as X-Session-Token
POST /v1/auth/logout                        # X-Session-Token
GET  /v1/auth/me                            # X-Session-Token → user
```

Staff identity gives role-per-request: `owner` can write expenses/pricing/series/courtblocks
and delete payments/customers; `pegawai` is read-mostly. The client token (`X-Client-Token`)
keeps full access for API/agent integration.

---

## 9. How to add a feature

### Shared feature (cross-tenant)

1. `cp -r internal/features/shared/menu internal/features/shared/<name>`
2. Edit `<name>/feature.go`: set `Name`, `Kind`, the validator.
3. Add the import in `cmd/server/main.go`: `_ "github.com/.../internal/features/shared/<name>"`
4. (Optional) add a default set in `DefaultSetFor(vertical)` — `internal/features/defaults.go`.
5. `go test ./...` + a curl smoke test.

### Custom feature (per tenant)

1. `cp -r internal/features/_template tenants/<slug>/backend/<name>`
2. Edit `feature.go`: `Name`, `Kind`, `VerticalSlugs`, `ValidatePayload`.
3. Edit `feature_test.go`: the **isolation test is required** — A creates, B gets 404.
4. Add the import in `cmd/server/main.go`.
5. `go test ./...`.

### Custom feature with its own table + routes (e.g. `sports/bookings`)

When generic records CRUD isn't enough (real columns, constraints, joins), the feature
provides `Routes func(chi.Router, features.BuilderDeps)`; the builder mounts it gated and the
feature owns its handlers.

1. Migration `internal/db/migrations/NNN_<name>.sql`: table + RLS policy + `GRANT ... TO app_runtime`.
2. `feature.go`: `Kind: ""` (not a `tenant_records` kind), `Routes: routes`.
3. Handlers use `records.WithTenantTx` (sets `app.tenant_id` for RLS) + `records.RequireClient` / `WriteJSON`.
4. Feature test: isolation **and** the feature's invariant (bookings has a real double-booking concurrency test).
5. Add `app` role grants for the new tables to `bootstrapAppRole` in every test harness.

### Vertical 13+

Append the slug to `AllowedVerticals` in `internal/features/builder.go`, and add a default
feature set in `internal/features/defaults.go` if it should differ.

### Where to edit what

| Change | Files |
|---|---|
| `/v1/chat` | `internal/api/chat.go` + `internal/llm/client.go` + `internal/quota/` |
| `/v1/{vertical}/records` | `internal/features/shared/records/handlers.go` + migrations |
| `/v1/{vertical}/{feature}` | a package under `internal/features/shared/<name>/` or `tenants/<slug>/backend/<name>/` |
| Cache / rate limit | `internal/cache/`, `internal/middleware/ratelimit.go`, `internal/redisx/` |
| Delivery / reminders | `internal/notify/`, `internal/reminder/`, `ops/schedule-delivery.sql` |
| Agent tools | `AgentsTools/` + `tenants/<slug>/{tools,memory,skills}/` per tenant |

---

## 10. Deploy

Two supported shapes. Every background job is **claim-based, not process-local**, so both work.

### Vercel + Supabase (serverless)

The Go runtime builds a native Linux binary from `cmd/server/main.go`. Set
**`BACKGROUND_TICKERS=false`** and Supabase owns the clock: `pg_cron` calls the
`deliver-notification` Edge Function, and a Database Webhook on `notify_jobs` INSERT gives
booking confirmations instant delivery with a cron backstop. (Reminder delivery
via `deliver-reminders` is PENDING-REPAIR — see `ops/RUNBOOK-delivery.md`.)

Set **`DB_POOL_MAX=2`** (default 20 is *per instance*) and **`TRUSTED_PROXY_CIDRS`** to Vercel's
egress ranges — without the latter, `X-Forwarded-For` is ignored, every client buckets as one
IP, and you rate-limit yourself into `429`s.

- `ops/RUNBOOK-vercel.md` — env vars, deploy, cold-start cost
- `ops/RUNBOOK-delivery.md` — Edge Functions, `pg_cron`, Vault, verification

> ⚠️ Supabase's free tier **pauses projects idle for 7 days**, and a paused project stops
> `pg_cron` — reminders silently stop. Either ping it externally or use a paid plan.

### Railway / VPS (persistent container)

- **Railway:** `railway up`, env (`DATABASE_URL` = Supabase `6543` TX pooler, `REDIS_URL` =
  Redis Cloud or Upstash), healthcheck `GET /healthz`
- **VPS (Hostinger KVM):** `docker compose up -d`
- Leave `BACKGROUND_TICKERS` unset — the Go ticker owns delivery, no Supabase setup needed.

### Docker images

`Dockerfile` builds for the Buildx target arch (`ARG TARGETARCH` → `GOARCH`).
Tagged pushes build `linux/amd64` **and** `linux/arm64` via `release.yml`.

---

## 11. Operations

- `RUNBOOK.md` — health, log queries, incident playbooks (429 spikes, LLM 502s, Postgres down,
  Redis down, Sentry), rollback, client onboarding, migrations.
- `ops/backup.sh` — nightly `pg_dump` with pruning. **Not currently scheduled** — wire it to a
  GitHub Actions cron (refinementNotes R18).
- `Makefile` — `run`, `build`, `test`, `vet`, `fmt`, `tidy`, `docker`, `compose-up/down`,
  `healthz`, `redis-cli`.

---

## 12. Known gaps

- **Streaming SSE** for `/v1/chat` and the agent endpoints — the longest-standing gap and the
  largest UX win available.
- **No OpenAPI spec** and **no coverage threshold in CI** — the contract and the quality gates
  are both prose-only. Tracked as C2/C4.
- **Cost accounting is an estimate**, and prompt-cache tokens are not read at all — the largest
  cost lever in the repo is currently unmeasured. Tracked as R6.
- **WhatsApp delivery is a `wa.me` deep link** — a booking confirmation does not actually reach
  a customer anywhere yet. Tracked as R15.
- **Delivery logic exists twice** (Go goroutines *and* Deno Edge Functions) with no parity test.
- **No audit log** of who changed what.

Full list, with severity and remediation, in [`docs/refinementNotes.md`](docs/refinementNotes.md).

---

## 13. Docs in this repo

| Doc | Purpose |
|---|---|
| `README.md` | This file — what the project is, how to run it, the endpoint surface |
| `ARCHITECTURE.md` | System design + tech stack: why each version, RLS rules, feature platform, delivery architecture |
| `docs/refinementNotes.md` | Critical review, current-state baseline, prioritized recommendations, roadmap |
| `RUNBOOK.md` | Operational incident playbooks (kept as-is) |
| `ops/RUNBOOK-delivery.md`, `ops/RUNBOOK-vercel.md` | Deployment-specific delivery and Vercel setup |
| `internal/features/**/README.md` | Per-feature notes, colocated with code |
| `tenants/<slug>/{memory,skills,tools}/` | **Runtime content**, embedded by `tenantembed.go` — not documentation |