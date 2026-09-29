# AgentsProxy — DFD Single Go Monolith (AI + Business)

> One deployed binary for all DFD agentic clients. `POST /v1/chat` (quota-metered LLM proxy) + `POST/GET /v1/{vertical}/*` (tenant CRUD, Supabase Postgres RLS) + `POST /v1/admin/*` (create token+quota). Client sites are static (Hostinger) — they call this.

**Stack:** Go **1.27** + `chi/v5` + `pgx/v5/pgxpool` + `go-redis/v9` · **DB:** One Supabase Postgres 17 (tenancy) · **Cache:** Redis 8.10.1 · **Deploy:** Vercel (Go runtime) + Supabase, $0 — or Railway/VPS for a persistent container.

---

## Quick Start (Laragon — Your Daily Dev, Not Docker)

1.  Copy env:
    ```bash
    copy .env.example .env
    # Edit: DATABASE_URL (postgres://postgres@localhost:5432/agents_proxy?sslmode=disable for Laragon)
    #       REDIS_URL=localhost:6379
    #       LLM_API_KEY=sk-... (DeepSeek/mimo), LLM_BASE_URL=https://api.deepseek.com
    #       ADMIN_API_KEY=$(openssl rand -hex 32) — generate once, same value in your Langchain dfd_bridge
    ```
2.  Laragon → **Start All** → ensure `Postgres` (5432) + `Redis` (6379) green (Services). Create DB `agents_proxy` in HeidiSQL if first run.
3.  Run (from `D:\MyProject\AgentsProxy`):
    ```bash
    go mod tidy
    go run ./cmd/server
    # Migrations auto-run on boot. Server on :8080.
    ```
4.  Verify:
    ```bash
    curl -s http://localhost:8080/healthz
    # {"status":"ok","version":"dev","db":true,"redis":true}
    go vet ./... && go test ./... -cover
    ```
5.  HeidiSQL → `SELECT * FROM clients;` — `dev` seeded. `redis-cli KEYS "ck:*" "rl:*" "quota:*"`.

> **Prod only** — `docker compose up --build` (postgres:16 + redis:8 + app) is for Railway/VPS. Not for local.

---

## Env

See `.env.example` (every var has a comment). Required: `DATABASE_URL`, `REDIS_URL`, `LLM_API_KEY`, `ADMIN_API_KEY`.

| Var | Default | Notes |
|---|---|---|
| `PORT` | `8080` | |
| `DATABASE_URL` | — (required) | Laragon local: `postgres://postgres@localhost:5432/agents_proxy?sslmode=disable` |
| `REDIS_URL` | `localhost:6379` | |
| `LLM_BASE_URL` | `https://api.deepseek.com` | Provider-swappable |
| `LLM_API_KEY` | — (required) | Secret |
| `LLM_MODEL` | `deepseek-chat` | |
| `ADMIN_API_KEY` | — (required) | `Authorization: Bearer $ADMIN_API_KEY` |
| `MAX_TOKENS_PER_REQUEST` | `2048` | |
| `RATE_LIMIT_PER_MINUTE` | `60` | |
| `ALLOW_ORIGINS` | `*` dev | Prod: `https://a.my.id,https://b.my.id` |
| `ENV` | `development` | `production` tightens CORS |

---

## Endpoints (Full Contract in PRD §5–7 + §6.5)

| Method | Path | Auth | Body / Query |
|---|---|---|---|
| `GET` | `/healthz` | none | — |
| `POST` | `/v1/chat` | `client_token` in body | `{"client_token":"...","messages":[{"role":"user","content":"..."}],"model":"..."}` |
| `POST` | `/v1/{vertical}/records` | `X-Client-Token` | `{kind, title, payload}` |
| `GET` | `/v1/{vertical}/records?kind=&q=&before=&limit=&status=` | `X-Client-Token` | cursor pagination; `status=active` default, `status=deleted` to see tombstones |
| `GET` | `/v1/{vertical}/records/{id}` | `X-Client-Token` | — |
| `PATCH` | `/v1/{vertical}/records/{id}` | `X-Client-Token` | `{title?, payload?, status?}` |
| `DELETE` | `/v1/{vertical}/records/{id}` | `X-Client-Token` | — (soft; `status=deleted`) |
| `POST` | `/v1/{vertical}/{feature}` | `X-Client-Token` | feature payload (validated) — `feature` ∈ the client's `features[]` |
| `GET` | `/v1/{vertical}/{feature}` | `X-Client-Token` | same list shape as records |
| `GET` | `/v1/{vertical}/{feature}/{id}` | `X-Client-Token` | — |
| `PATCH` | `/v1/{vertical}/{feature}/{id}` | `X-Client-Token` | feature payload (validated) |
| `DELETE` | `/v1/{vertical}/{feature}/{id}` | `X-Client-Token` | — (soft) |
| `POST` | `/v1/admin/clients` | `Bearer ADMIN_API_KEY` | `{name, tenant_id, monthly_quota_tokens?, vertical?, features?}` |
| `GET` | `/v1/admin/clients` | `Bearer ADMIN_API_KEY` | — |
| `PATCH` | `/v1/admin/clients/{id}` | `Bearer ADMIN_API_KEY` | `{quota?, status?, vertical?, features?}` (cache evicted) |
| `GET` | `/v1/admin/usage` | `Bearer ADMIN_API_KEY` | `?from=&to=&client=` |

Allowed `vertical`: `fnb,salon,laundry,klinik,gym,bimbel,bengkel,beauty,travel,property,katering,photo,sports`.
Shared `feature`s: `records` (always on), `menu`, `booking`, `review`, `article`, `promo`. Custom features added per-tenant live at `internal/features/clients/<slug>/`.

### Sports vertical (Phase 4)

```
POST   /v1/sports/facilities                      create a court/field/pool (name, type, capacity, price_per_hour, open_time, close_time)
GET    /v1/sports/facilities                      list
POST   /v1/sports/bookings                        book a slot (409 "slot already booked" on overlap — DB-enforced)
GET    /v1/sports/bookings?date=YYYY-MM-DD        daily schedule view
POST   /v1/sports/inventory/{id}/adjust           {delta} — restock (+N) / sale (−N), never negative
POST   /v1/sports/members/{id}/topup              {amount} — prepaid balance top-up
```

The double-booking guarantee is at the **database** level: `sports_bookings` has an exclusion constraint over `(tenant_id, facility_id, tsrange(date+start, date+end))`, so no application bug can overlap a court's time. Confirmations enqueue a WhatsApp notification via `internal/notify/` (wa.me deep link by default; `Sender` interface ready for Meta/Twilio).

A sports client needs features `facilities`, `bookings`, `inventory`, `members` — enable via `PATCH /v1/admin/clients/{id}`.

## Telegram multi-bot layer (1 bot per role per tenant)

Thousands of tenants, thousands of bots: bot tokens live encrypted in
`telegram_bots` (AES-GCM, one `TELEGRAM_VAULT_KEY` in env — never in
`.env` per bot). Webhooks in prod (`PROXY_PUBLIC_URL`), per-bot
polling fallback for dev (`TELEGRAM_POLL_DEV=1`).

```
# 1. Create a bot via @BotFather → token (one per role: Owner, Kasir, Info)
# 2. Register it (owner session; token validated via getMe, webhook set):
POST /v1/sports/telegram/bots
  {"name":"Owner","role":"owner","token":"123456:ABC...","allow_chat_ids":[987654321]}
#    owner/pegawai bots REQUIRE allow_chat_ids (strangers refused);
#    client bots are open FAQ. Tokens are never returned (masked).

# 3. Owner messages the bot:
"book Court 3 tomorrow 19:00"      → books (409 → agent offers alternatives)
"income this month?"               → SUM of confirmed bookings
"remind me Friday 8am to order shuttlecocks" → reminder, sent via the same bot
"buatkan laporan excel bulan ini"  → .xlsx document attachment
```

Tenant + role come from the bot's DB row (webhook secret verified) —
never from request params. History/RPD/reminders are namespaced per
chat per bot. Without `TELEGRAM_VAULT_KEY`, bots stay disabled and
normal REST clients are unaffected.

### Phase 6 — Centrum migration features (money + schedule)

```
POST   /v1/sports/payments          record a cash/QRIS payment (payment_status auto-recalcs: unpaid/partial/paid)
GET    /v1/sports/payments?booking_id=  payments for a booking
POST   /v1/sports/pricing           date-aware pricing rule (day/evening rates, open/close)
GET    /v1/sports/pricing           list rules (bookings auto-compute price from the active rule)
POST   /v1/sports/expenses          record an expense (category, note)
POST   /v1/sports/customers         customer directory (no-show count, visit aggregation)
POST   /v1/sports/courtblocks       block a court / all courts (maintenance, private events)
POST   /v1/sports/series            recurring weekly booking (community groups)
POST   /v1/sports/kantin/products   canteen product (cost + stock + low-stock threshold)
POST   /v1/sports/kantin/sales      ATOMIC sale — total server-computed, stock decremented, movements logged
```

Bookings now support the full lifecycle (`pending → confirmed → checked_in → completed / cancelled / no_show`), `source` (whatsapp/walk_in), `override_price`, and `payment_status`. A cancelled/no-show slot is rebookable (exclusion constraint ignores those statuses).

### Staff login (Phase 7) — no Supabase

```
POST /v1/auth/login   {"tenant_id":"centrum","email":"owner@...","password":"..."}
  → {"token":"<session>", "user":{...}}     # send as X-Session-Token
POST /v1/auth/logout                        # X-Session-Token
GET  /v1/auth/me                            # X-Session-Token → user
```

Staff identity (`X-Session-Token`) gives role-per-request: `owner` can write expenses/pricing/series/courtblocks + delete payments/customers; `pegawai` is read-mostly. The client token (`X-Client-Token`) keeps full access for API/agent integration. Availability for the landing page: `GET /v1/sports/availability?date=`.

---

## How to Add a Feature

### Shared feature (cross-tenant)

1. `cp -r internal/features/shared/menu internal/features/shared/<name>`
2. Edit `<name>/feature.go`: set `Name`, `Kind`, the validator.
3. Add an import in `cmd/server/main.go`:
   ```go
   _ "github.com/.../internal/features/shared/<name>"
   ```
4. (Optional) Add the feature to `DefaultSetFor(vertical)` in `internal/features/defaults.go`.
5. `go test ./...` and a curl smoke test.

### Custom feature (per tenant)

1. `cp -r internal/features/clients/_template internal/features/clients/<tenant-slug>`
2. Edit `feature.go`: set `Name`, `Kind`, `VerticalSlugs`, `OwnerTenant`, `ValidatePayload`.
3. Edit `feature_test.go`: the **isolation test is required** — A creates, B gets 404.
4. Edit `README.md`: status, owner, why.
5. Add the import in `cmd/server/main.go`.
6. `go test ./...`.

### Custom feature WITH its own table + routes (e.g. `sports/bookings`)

When generic records CRUD isn't enough (real columns, constraints, joins), the feature provides its own `Routes func(chi.Router, features.BuilderDeps)` — the builder mounts it gated, and the feature owns its handlers. Pattern (see `internal/features/clients/sports/`):

1. Migration `internal/db/migrations/NNN_<name>.sql`: table + RLS policy + `GRANT ... TO app_runtime`.
2. `feature.go`: `Kind: ""` (not a tenant_records kind), `Routes: routes`.
3. Handlers use `records.WithTenantTx` (sets `app.tenant_id` for RLS) + `records.RequireClient` / `WriteJSON`.
4. Feature test: isolation + the feature's invariant (sports bookings has a double-booking concurrency test).
5. Add the `app` role grants for the new tables to `bootstrapAppRole` in every test harness.

### Vertical 13

Append the new slug to `AllowedVerticals` in `internal/features/builder.go` and add a default feature set in `internal/features/defaults.go` if the vertical should get different defaults.

## Maintaining the Monolith

- `/v1/chat` → touch `api/chat.go` + `llm/client.go` + `quota/quota.go`.
- `/v1/{vertical}/records` → touch `internal/features/shared/records/handlers.go` + `migrations/`.
- `/v1/{vertical}/{feature}` → add/edit a package under `internal/features/shared/<name>/` or `internal/features/clients/<slug>/`.
- Cache/rate-limit → `cache/`, `middleware/ratelimit.go`, `redisx/`.

## Deploy

Two supported shapes, both $0. Pick per environment — the code works
either way because every background job is claim-based, not
process-local.

### Vercel + Supabase (serverless)

The Go runtime builds a **native Linux binary** (not Wasm) from
`cmd/server/main.go`; Vercel sets `GOOS=linux` and downloads the
toolchain named in `go.mod`. The service is a **separate Vercel project
from every client frontend** so platform secrets (`LLM_API_KEY`,
`TELEGRAM_VAULT_KEY`, `DATABASE_URL`) are never in a frontend's env
namespace.

Serverless destroys the process between requests, so the in-process
background loops cannot run. Set **`BACKGROUND_TICKERS=false`** and
Supabase owns the clock: `pg_cron` calls a `deliver-reminders` Edge
Function, and a Database Webhook on `notify_jobs` INSERT gives booking
confirmations instant delivery with a cron backstop.

- `ops/RUNBOOK-vercel.md` — env vars, deploy, what the cold start costs
- `ops/RUNBOOK-delivery.md` — Edge Functions, `pg_cron`, Vault, verification

Two things to get right, both silent when wrong:

- `DB_POOL_MAX=2` (the default 20 is **per instance**; 20 concurrent
  lambdas would open 400 connections into Supabase)
- `TRUSTED_PROXY_CIDRS` must include Vercel's egress ranges, or
  `X-Forwarded-For` is ignored, every client buckets as one IP, and you
  rate-limit yourself into `429`s

The old note "NOT Vercel (duration limits kill long LLM calls)" is
**obsolete** — Hobby now allows 300s, well past this service's 60s
`llm.Client` cap.

### Railway / VPS / Hostinger (persistent container)

- **Railway:** `railway up`, env (`DATABASE_URL` = Supabase `6543` TX
  pooler, `REDIS_URL` = Redis Cloud or Upstash), healthcheck `GET /healthz`
- **VPS (Hostinger KVM):** `docker compose up -d`
- Leave `BACKGROUND_TICKERS` unset — the Go ticker owns delivery here and no
  Supabase setup is needed

### Docker images

`Dockerfile:14` hardcodes `GOARCH=amd64`. Oracle Cloud's Ampere A1 (the
free always-on tier) is ARM64 — build with `ARG TARGETARCH` and
`buildx` if you deploy there.

## Known Gaps

- Streaming SSE (`Accept: text/event-stream`) is pass 2 after non-streaming is green.

---

## Docs in This Repo

- `PRD.md` — master executable prompt (what to build, how to execute Phase 1 → Phase 2)
- `ARCHITECTURE.md` — why latest versions (Go 1.27 / Redis 8.10 / PG17 + RLS) and tradeoffs
- `STANDARDS.md` — golden standard quality gates

