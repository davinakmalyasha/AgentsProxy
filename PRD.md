# PRD — D:\MyProject\AgentsProxy (Go Monolithic AI + Business Proxy)

> **Status:** Master executable prompt — other agents execute this file. Do not deviate without updating this PRD.
> **Path:** `D:\MyProject\AgentsProxy` (Windows; `cd` there for every command; never `D:/AgentsProxy`).
> **Language:** Go 1.27 (latest stable pinned at build time) · **Cache:** Redis 8.10.1 · **DB:** Supabase Postgres 17 (one shared project)
> **Deploy:** Railway/VPS via Dockerfile (persistent container) — NOT Vercel serverless (LLM calls exceed 10s serverless limits).

---

## 1. Context — Why This Service Exists

### 1.1 The Triangle

```
LangChain (D:\MyProject\Langchain) — FACTORY, private, your laptop + phone via Tailscale
  DFD workspace _dfd + dfd_bridge (milestones) + proxy_bridge (create token) — private, scopes=["dfd"] only
        | create-client-token (ADMIN_API_KEY)          | milestones/notes (DFD_API_KEY)
        v                                              v
Go Proxy (D:\MyProject\AgentsProxy, deployed)     DFD Backend (D:\MyProject\DFDWebsite/server, Express 5 on Railway + MySQL Aiven)
  POST /v1/chat per-client token                      Prisma + shared schemas + WhatsApp
        \                                               ▲ same Supabase Postgres (shared DB, RLS)
         \ ⑤ chat widget calls (NEXT_PUBLIC_AGENTS_PROXY_URL + per-client token)
          v
Client Sites (Hostinger Unlimited static, 12 verticals from D:\MyProject\MainTemplates → DFDClientProjects) → Supabase (tenancy)
```

- **You own the templates warehouse** (`D:\MyProject\MainTemplates` → 12 verticals, `common/agentic-ui` thin widgets). Its sites call **this proxy** for AI. You do that part yourself; this repo does NOT build templates.
- **Agents are on top (factory).** Proxies are in the middle (runtime, deployed, always-on). Client sites are at the bottom (Hostinger static, no per-client server). The proxy being "unified" means **one deployed instance serves all clients** — not one per client.

### 1.2 Why This Proxy Exists (The Business Reason)

Every agentic client subscription includes "unlimited agentic possibility" at ~$10/mo provider cost folded into Rp1.5M build renewal. The client **must never hold the master LLM key**. They hold a per-client token; you hold the master key on the server. This service:

1.  Holds **THE** DeepSeek/mimo key (env `LLM_API_KEY`) — never in repos, never in client sites.
2.  Meters **per-client usage** (monthly quota + sliding-window rate limit) — cheap model year 2 (10% credit, mimo).
3.  Logs everything for billing/dashboard — DFD backend reads the same Postgres to show usage.

### 1.3 Why Go Here, Not Node

The DFD backend stays Node (Express 5 + Prisma + `whatsapp-web.js` + `shared/` Zod). Rewriting it in Go is weeks of risk for zero user gain. **This proxy is the ONLY Go candidate** — it's a 500-line gateway where Go genuinely wins: single static binary (~15MB), `log/slog` JSON, `net/http` enhanced routing, Green Tea GC -35% + Swiss Tables -2-3% free perf, container-aware `GOMAXPROCS`.

---

## 2. Goals & Non-Goals

### Goals

1.  `go build ./...` clean, `go vet ./...` clean, `go test ./...` green (≥14 tests) with `pgx` + `go-redis` against real Postgres+Redis (Laragon dev) or `miniredis` in CI.
2.  `docker compose up --build` brings `app + postgres:16 + redis:8` locally; `curl /healthz` = `{"status":"ok"}` with DB+Redis pings.
3.  Monetizable: per-client monthly quota enforced server-side, pay-as-you-go AI (10% credit), host/domain renewal model (`200k/500k/1.5M` build includes Yr1, renewal = host+AI).
4.  Hostinger Unlimited holds **all client statics** (200 sites) — zero per-client infra, one Supabase project for all data.

### Non-Goals (Explicit)

- This repo does **not** build `D:\MyProject\MainTemplates` (other agents do that from `docs/dfd-templates-build-prompt.md`).
- This repo does **not** manage DFD milestones/orders — that's `dfd_bridge` in Langchain.
- No per-client DB, no per-client server, no per-client Redis keyspace beyond `ck:{hash}`/`quota:{id}`/`rl:{id}`.
- No Vercel proxy deploy (duration limits kill long LLM calls).

---

## 3. What to Deliver (Exact Tree — Create This)

```
D:\MyProject\AgentsProxy\
├── README.md                    # §9 — Quick start (Laragon, not Docker for dev)
├── PRD.md                      # THIS FILE (master — context + deliver + steps)
├── ARCHITECTURE.md             # § — Latest-version deep dive (Go 1.27 / Redis 8.10 / PG17)
├── STANDARDS.md                # Golden standard (quality gates for this Go repo)
├── .env.example                # DOCUMENT every var with a comment block
├── .gitignore                  # .env, bin/, *.log, .air.toml
├── Dockerfile                  # multi-stage → scratch/static ~15MB, CA certs copied
├── docker-compose.yml          # PROD ONLY: app + postgres:16-alpine + redis:8-alpine, healthchecks
├── Makefile                    # run, build, test, vet, tidy, docker, redis-cli
├── go.mod / go.sum             # github.com/davinakmalyasha/agents-proxy, pin Go 1.27 minor
├── cmd/server/main.go          # load env, open pgxpool (Min 2 Max 20) + redis, run migrations, start chi/std mux :8080, handle SIGINT→Shutdown(10s)
├── internal/
│   ├── config/config.go        # PORT(8080), DATABASE_URL(required), REDIS_URL(default localhost:6379 for Laragon),
│   │                           # LLM_BASE_URL(default https://api.deepseek.com), LLM_API_KEY(required),
│   │                           # LLM_MODEL(default deepseek-chat), ADMIN_API_KEY(required),
│   │                           # MAX_TOKENS_PER_REQUEST(int, default 2048), RATE_LIMIT_PER_MINUTE(int, default 60),
│   │                           # ALLOW_ORIGINS(csv, default * for dev, document prod override to explicit domains)
│   ├── db/db.go                # pgxpool.Pool singleton (MinConns 2 MaxConns 20, MaxConnLifetime 1h, HealthCheck 1m), Ping, Close
│   ├── db/migrate.go           # embed migrations/*.sql via go:embed; apply in order idempotently in a single TX per file (SELECT FROM schema_migrations)
│   ├── redisx/client.go        # go-redis/v9 singleton (*redis.Client), Ping, Close, FlushDB helper for tests
│   ├── cache/client.go         # GetClientByHash(hash) → {id, tenant_id, status, monthly_quota_tokens}: GET ck:{hash} (JSON, TTL 900); miss→ SELECT clients WHERE token_hash=$1, SETEX 900; Evict on PATCH status
│   ├── middleware/
│   │   ├── cors.go             # chi/cors or stdlib; ALLOW_ORIGINS csv (preflight OPTIONS handled, credentials off)
│   │   ├── logging.go          # slog JSON: method,path,status,duration_ms,client_id; NEVER log tokens/bodies; ReplaceAttr redacts Authorization
│   │   ├── ratelimit.go        # Redis INCR rl:{id}:{YYYYMMDDHHmm} + EXPIRE 60; if > RATE_LIMIT_PER_MINUTE → 429 + Retry-After
│   │   └── recovery.go         # recover → 500 {"error":"internal"}
│   ├── auth/
│   │   ├── tokens.go           # GenerateToken() = crypto/rand 32 bytes hex(64 chars); HashToken = hex(sha256(token)); Equal = subtle.ConstantTimeCompare
│   │   └── admin.go            # RequireAdmin(next): Authorization: Bearer $ADMIN_API_KEY → 401 if missing/wrong
│   ├── quota/quota.go          # Check(ctx, pool, redis, client) → (remaining int, resetAt time.Time, err quotaExceeded). Uses cache §4: GET quota:{id}:{YYYYMM} or DB SUM, then SETEX 30s
│   ├── llm/client.go           # OpenAI-compatible POST {baseURL}/chat/completions — body {model, messages, max_tokens, stream}. 60s timeout. DO NOT LOG prompt. Sanitize provider errors.
│   ├── usage/store.go          # Record(ctx, pool, redis, clientID, tokensIn, tokensOut, model, kind ∈ {chat,business})
│   └── api/
│       ├── healthz.go          # GET /healthz → {"status":"ok","version":"dev"}; pings DB (SELECT 1) + Redis PING; 500 if either down. Distinct from /readyz if added.
│       ├── chat.go             # POST /v1/chat (§5) — AI proxy — Phase 1
│       ├── business.go         # Generic CRUD: POST/GET/PATCH/DELETE /v1/{vertical}/records (§6) — Phase 2 monolith, same service
│       ├── admin.go            # POST /v1/admin/clients, PATCH /v1/admin/clients/{id}, GET /v1/admin/usage (§7)
│       └── router.go           # chi.NewRouter() or stdlib ServeMux (if flat): Use(cors, logging, ratelimit, recovery); mount healthz, chat, business groups, admin
├── migrations/
│   ├── 001_clients_usage.sql   # §3.1 — clients + usage
│   ├── 002_tenancy.sql         # §3.2 — tenant_records + RLS
│   └── 003_seed_dev.sql        # §3.3 — dev client (name='dev', tenant_id='dev', token_hash=sha256Hex('dev-token'))
└── (tests colocated *_test.go: api/*_test.go, middleware/*_test.go, quota/*_test.go)
```

> **File count invariant:** No file outside this tree without updating this PRD.

---

## 4. Data Model (One Postgres — All Tenants)

### 4.1 `migrations/001_clients_usage.sql` — Apply in a single transaction

```sql
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS schema_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());

CREATE TABLE IF NOT EXISTS clients (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name            text NOT NULL,
  tenant_id       text NOT NULL UNIQUE,           -- slug e.g. 'warung-sari', also used for RLS: SET LOCAL app.tenant_id
  token_hash      text NOT NULL UNIQUE,           -- sha256Hex(per-client token), never raw token
  monthly_quota_tokens integer NOT NULL DEFAULT 500000,
  status          text NOT NULL DEFAULT 'active' CHECK (status IN ('active','paused','blocked')),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON clients(token_hash);
CREATE INDEX ON clients(tenant_id);

CREATE TABLE IF NOT EXISTS usage (
  id              bigserial PRIMARY KEY,
  client_id       uuid NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
  tokens_in       integer NOT NULL DEFAULT 0,
  tokens_out      integer NOT NULL DEFAULT 0,
  cost_usd        numeric(10,6) NOT NULL DEFAULT 0,
  model           text NOT NULL DEFAULT '',
  kind            text NOT NULL CHECK (kind IN ('chat','business')),
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON usage(client_id, created_at);
CREATE INDEX ON usage(created_at);
CREATE INDEX ON usage(kind);
```

### 4.2 `migrations/002_tenancy.sql` — Monolithic business table (virtualized verticals — no DDL for vertical 13)

```sql
CREATE TABLE IF NOT EXISTS tenant_records (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  vertical      text NOT NULL,                    -- 'fnb','salon','laundry','klinik','gym','bimbel','bengkel','beauty','travel','property','katering','photo'
  kind          text NOT NULL,                    -- 'item','service','booking','order','doctor','listing','package', etc.
  title         text NOT NULL DEFAULT '',
  slug          text NOT NULL DEFAULT '',
  payload       jsonb NOT NULL DEFAULT '{}'::jsonb, -- all vertical-specific fields (menu price, doctor specialty, listing beds, etc.)
  status        text NOT NULL DEFAULT 'active',
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON tenant_records(tenant_id, vertical, kind);
CREATE INDEX ON tenant_records(slug);
CREATE INDEX ON tenant_records USING GIN (payload); -- for q search
ALTER TABLE tenant_records ENABLE ROW LEVEL SECURITY;
-- Defense in depth — proxy also enforces tenant_id in Go via SET LOCAL
CREATE POLICY tenant_isolation ON tenant_records
  FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));
```

### 4.3 `migrations/003_seed_dev.sql`

```sql
-- token = 'dev-token', hash = sha256Hex('dev-token')
INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status)
VALUES ('dev', 'dev', encode(digest('dev-token','sha256'),'hex'), 1000000, 'active')
ON CONFLICT (tenant_id) DO NOTHING;
```

**Extensibility note (inside README too):** Add vertical 13 = add one string to `allowedVerticals` in `business.go`. Add a custom feature = a new `kind` value inside `payload` + a contract in docs. No new table, no new migration unless an indexed search field is requested (`CREATE INDEX ON tenant_records ((payload->>'field'))`).

**Rationale for generic `tenant_records` vs 12 tables:** 10–30 small businesses = hundreds of rows/month each, <10MB/client. 12 tables = 12 migrations × maintenance. One jsonb table + GIN index is the Stripe/Notion pattern for productized SaaS at this scale. When a vertical truly needs a dedicated table (millions of rows, complex joins), add it as `002_add_<vertical>.sql` — the monolith still serves it from the same deploy.

---

## 5. Redis Architecture (Hot Layer — Source of Truth Stays Postgres)

### Why Redis at All

Postgres `SUM(usage)` on every chat = unnecessary DB load. Redis is **not a replacement** — it's a hot cache that makes the proxy fast. Truth is always Postgres.

### Keyspace

- `ck:{sha256(token)}` → JSON `{id, tenant_id, status, quota, name}` **TTL 900 (15m)**. On `PATCH /v1/admin/clients/{id}` with new quota/status, `DEL ck:*` for that client (or `UNLINK`).
- `rl:{clientID}:{YYYYMMDDHHmm}` → `INCR` counter, `EXPIRE 60`. One key per minute per client. Old keys auto-expire — no cleanup job.
- `quota:{clientID}:{YYYYMM}` → cached integer `SUM(tokens_in+tokens_out)` for current month, **TTL 30**. Invalidated on each `usage` INSERT (`DECRBY`/`INCRBY` best-effort) + TTL safety net. If missing, middleware computes from DB then `SETEX 30`.

### Flows (use what the owner installed: Redis 8.10)

- **Auth cache (cache/client.go):** `GET ck:{hash}` → hit = use; miss → `SELECT id, tenant_id, status, monthly_quota_tokens FROM clients WHERE token_hash=$1` (pgx) → `SETEX ck:{hash} 900 <json>` → return.
- **Rate limit (middleware/ratelimit.go):** `INCR rl:{id}:{minute}` → current++; if `current == 1` then `EXPIRE 60`. If `current > RATE_LIMIT_PER_MINUTE` → `429` with `Retry-After: <60 - secInMinute>`. Optional Redis 8 `HGETEX`/`HEXPIRE` alternative (1 RTT) — document as future improvement.
- **Quota (quota/quota.go):** `GET quota:{id}:{month}` if exists else DB `SELECT COALESCE(SUM(tokens_in+tokens_out),0) FROM usage WHERE client_id=$1 AND created_at >= date_trunc('month', now())` then `SETEX 30`. Compare to `monthly_quota_tokens`. If exceeded → `429 {"error":"quota exceeded","resetAt":"<first of next month ISO>"}`.
- **NEVER store raw tokens in Redis.** Only `sha256Hex(token)` as the key; the value is the hashed-id mapping. Never log tokens or bodies (`slog` `ReplaceAttr` redacts `Authorization`).

---

## 6. API Contracts

### 6.1 `POST /v1/chat` — AI Proxy (Phase 1 — Build FIRST, Make Green Before Business)

**Client → Proxy (public but token-authenticated):**
```json
{"client_token":"<per-client token>","messages":[{"role":"user","content":"..."}],"model":"optional-override"}
```
**Proxy → Provider (DeepSeek-compatible, server side, 60s timeout):**
`POST {LLM_BASE_URL}/chat/completions` with `Authorization: Bearer LLM_API_KEY`, body `{model, messages, max_tokens: MAX_TOKENS_PER_REQUEST}`. Do **NOT** log prompt or token.

**Handler steps (in order, no shortcuts):**

1.  Parse body, validate: `messages` non-empty, each `role ∈ {system,user,assistant}`, `content` non-empty, `client_token` required. Bad JSON / missing token → `400 {"error":"..."}`.
2.  Auth: `HashToken(client_token)` → `cache.GetClientByHash` (fallback DB on miss). Not found → `401 {"error":"invalid token"}`. `status != 'active'` → `403 {"error":"client paused"}`.
3.  Rate-limit block already ran in middleware (global). If not, check here.
4.  Quota: `quota.Check` → if exceeded `429 {"error":"quota exceeded","resetAt":"..."}` + `Retry-After`.
5.  Call provider (60s `context.WithTimeout`): capture `usage.prompt_tokens / completion_tokens` from provider if present; else estimate `char/4`. Compute `cost = tokens_in/1e6*0.14 + tokens_out/1e6*0.28` (DeepSeek flash estimate; document as estimate, not exact bill).
6.  Record: `usage.Record(ctx, pool, redis, clientID, tokensIn, tokensOut, model, kind='chat')` — INSERT to `usage` (synchronous, source of truth) + best-effort `INCRBY quota:{id}:{month}` in Redis.
7.  Return (normalized): `{"reply":"<assistant content>","usage":{"prompt_tokens":N,"completion_tokens":M,"cost_usd":0.00}}`. Sanitize provider errors (never echo raw; `{"error":"provider error","code":502}`).

Require: `subtle.ConstantTimeCompare` on hash, 60s `context.WithTimeout`, sanitize provider errors.

**Streaming (pass 2, after non-streaming is green):** If `Accept: text/event-stream`, proxy SSE `data:` frames from provider to client as-is (flush per chunk). Document as "Streaming pass" in README.

### 6.2 Business Monolith — `POST/GET/PATCH/DELETE /v1/{vertical}/records` (Phase 2, On Top of Green Phase 1) ✅

> Phase 3 reframe: §6.2 is the `records` **feature** in the feature platform
> (§6.5). Same URL shape, same auth, same RLS. New in Phase 3: cursor
> pagination (§9), soft-delete hidden by default, two-role split.

All business calls use header `X-Client-Token: <token>` (do **not** confuse with admin `Authorization: Bearer`). Validate `vertical` in the 12 allowed names (`fnb,salon,laundry,klinik,gym,bimbel,bengkel,beauty,travel,property,katering,photo`) — unknown → `400 {"error":"unknown vertical"}`.

| Method | Path | Body / Query | Effect |
|---|---|---|---|
| `POST` | `/v1/{vertical}/records` | `{kind, title, slug?, payload, status?}` | `SET LOCAL app.tenant_id='<tenant>'` then `INSERT` to `tenant_records`. `201 {"id":"..."}`. Validate `kind` non-empty. |
| `GET` | `/v1/{vertical}/records?kind=&q=&before=&limit=` | `kind` filter, `q` searches `title` + `payload` text (`ILIKE`), `before` is the cursor token from the previous page, `limit` default 20 max 100 | `SELECT ... WHERE tenant_id = caller AND vertical = $1 AND status = 'active' (+ kind/q) AND (created_at,id) < cursor ORDER BY created_at DESC, id DESC LIMIT N+1`. Response: `{"items":[...],"next_cursor":"<base64>","has_more":bool}`. Soft-deleted rows hidden by default; pass `?status=deleted` to see them. |
| `GET` | `/v1/{vertical}/records/{id}` | — | Single row owned by caller only (404 if not owned or soft-deleted). |
| `PATCH` | `/v1/{vertical}/records/{id}` | `{title?, slug?, payload?, status?}` (patch) | `UPDATE` own row, `SET LOCAL app.tenant_id`. 404 if not owned. |
| `DELETE` | `/v1/{vertical}/records/{id}` | — | Soft delete: `SET status='deleted'`. 404 if not owned. |

Before **every** DML: `SET LOCAL app.tenant_id = '<tenant>'` so RLS applies even if handler forgets the `WHERE`. Record `usage` with `kind='business'` (small token estimate, e.g. `len(payload)/4`, for analytics — not billed against LLM quota unless you choose to).

**Extensibility (inside README):** Add vertical 13 → add one string to `allowedVerticals` in `business.go`. Add custom feature → new `kind` value inside `payload` + contract in docs. No DDL unless an indexed search field is requested (`CREATE INDEX ON tenant_records ((payload->>'field'))`).

### 6.5 Feature Platform ✅

Every capability a client can turn on is a **feature** registered in `internal/features/registry.go`. The platform has three layers:

- **Core** — `records` (the §6.2 CRUD engine). Always available, never gated.
- **Shared** — `menu`, `booking`, `review`, `article`, `promo`. Each writes a row to `tenant_records` with a fixed `kind` (e.g. `menu_item`) and enforces a payload contract in `ValidatePayload`. Defined under `internal/features/shared/<name>/`.
- **Custom** — per-tenant. Defined under `internal/features/clients/<tenant-slug>/` by copying `_template/`. Each gets its own folder, its own `feature.go`, and a **mandatory** isolation test.

URL shape: `/v1/{vertical}/{feature}/...`. The `records` feature retains its `/{vertical}/records/...` URL for backwards compat.

Gate: `RequireFeature(name)` middleware reads the cached `ClientModel` (set by `withClientAuth`) and rejects 403 if `name ∉ client.features` or `!AllowedForVertical(name, client.vertical)`. **Zero extra DB hit.**

Admin endpoints: `POST /v1/admin/clients` accepts optional `vertical` (default `fnb`) and `features` (default = `DefaultSetFor(vertical)`); unknown feature name → 400. `PATCH` accepts `features` (array) and evicts the cache. `GET /v1/admin/clients` lists every client with `id, name, tenant_id, vertical, features, status, monthly_quota_tokens, created_at`.

Tiers and promotion: a custom feature becomes a candidate for shared promotion after it has been live for 2+ months and used by 3+ tenants. Promotion is a code move, not a schema change.

### 6.6 Sports Vertical ✅ (Phase 4 — "strengthen first" pilot)

The first fully-strengthened vertical. **Sports venue booking** (badminton, soccer, swimming, padel). It is the pilot that proves the custom-table feature path (the unproven part of §6.5) and the hardest concurrency problem on the platform: **double-booking prevention**.

Four custom features live at `internal/features/clients/sports/`:

| Feature | Table | Why a real table (not `tenant_records`) |
|---|---|---|
| `facilities` | `sports_facilities` | Court/field config with TIME + NUMERIC columns, FK'd from bookings |
| `bookings` | `sports_bookings` | **Exclusion constraint** — the DB itself rejects overlapping bookings on the same facility |
| `inventory` | `sports_inventory` | Quantity tracking + low-stock threshold; atomic decrements, never negative |
| `members` | `sports_members` | Prepaid balance; atomic top-ups |

**The double-booking guarantee (the heart):** `sports_bookings` has

```sql
CONSTRAINT sports_bookings_no_overlap EXCLUDE USING gist (
  tenant_id WITH =, facility_id WITH =,
  tsrange(date + start_time, date + end_time) WITH &&
)
```

No application code can accidentally double-book, no matter how concurrent the requests. The half-open range `[start, end)` allows back-to-back (19:00–20:00 then 20:00–21:00) but rejects any overlap. Requires `CREATE EXTENSION btree_gist` (runs on the admin pool — migrations are superuser).

**WhatsApp notifications:** `internal/notify/` is a queue, not a direct call — a booking must never depend on WhatsApp being up. Confirmed booking → enqueue → worker sends via a `Sender` with retry + exponential backoff. `WaMeSender` (wa.me deep link, zero credentials) is the default; the `Sender` interface isolates a future Meta/Twilio swap to one file. Incoming-reply webhook (agentic) is a future extension point.

**Onboarding a sports client:**
```
POST /v1/admin/clients  { "tenant_id": "venue-x", "vertical": "sports" }
PATCH /v1/admin/clients/{id}  { "features": ["records","booking","review","promo","facilities","bookings","inventory","members"] }
```

### 6.7 Telegram Agentic Layer ✅ (Phase 5 — the "agentic upgrade" tier)

Same proxy, same DB, same RLS, same LLM key, same quota — the agent is a **conversational wrapper over the feature platform**, not a separate service. Sold as an upgrade: normal clients use REST only; agentic clients additionally get the `telegram` feature.

**Flow:** Telegram message → `agent_bindings` resolves chat→client → feature gate (`telegram` ∈ client.features) → `internal/agent` tool loop (≤5 rounds) → tools call the SAME business logic as REST (WithTenantTx + RLS + exclusion constraint) → reply via Telegram.

**Tools (sports):** `book_slot`, `cancel_booking`, `list_bookings`, `query_income`, `query_attendance`, `check_stock`, `adjust_stock`, `topup_member`, `record_staff_payment` (**ledger only — no money movement**), `list_staff_payments`, `set_reminder`.

**Reminders:** `agent_reminders` table + a scheduler goroutine (polls due rows, sends via Telegram). The inventory `adjust` handler hooks low-stock → auto-reminder.

**Config:** `TELEGRAM_BOT_TOKEN` (opt-in; unset = agent disabled). Bot uses `getUpdates` polling — zero infra, fine for one instance; webhook is the prod multi-instance upgrade.

**Security:** the agent runs per-tenant with RLS as the hard boundary — a prompt-injected message cannot cross tenants. Agent LLM calls bill the client's monthly quota via `usage.Record`.

### 6.8 Centrum Migration Features ✅ (Phase 6)

Phase 6 ported the real production features of CentrumBadminton (a live badminton-arena PWA) into the proxy's `sports` vertical, so the Next.js frontend can drop Supabase and talk to the proxy. Seven new custom-table features:

| Feature | Table | What it does |
|---|---|---|
| `payments` | `sports_payments` | Per-booking cash/QRIS; **DB trigger** recomputes `bookings.payment_status` (unpaid/partial/paid) |
| `pricing` | `sports_pricing_rules` | Date-aware day/evening rates + open hours; bookings auto-compute price |
| `expenses` | `sports_expenses` | Owner expense ledger (categories, notes) |
| `customers` | `sports_customers` | Directory + no-show count + visit aggregation |
| `courtblocks` | `sports_court_blocks` | Block a court (or all) for maintenance/private events |
| `series` | `sports_recurring_series` | Recurring weekly bookings (community groups) |
| `kantin` | `sports_kantin_*` | Canteen POS: products (cost+stock), **atomic sale** (server-computed total, stock never negative), stock movements |

**Bookings lifecycle** extended: `pending → confirmed → checked_in → completed / cancelled / no_show`, `source` (whatsapp/walk_in), `override_price`, `payment_status`. The exclusion constraint now ignores `cancelled`/`no_show` (a cancelled slot is rebookable).

**Deferred (documented):** Phase 2 = owner/pegawai auth in the proxy (single token in phase 1). Phase 3 = Telegram agent money tools (net profit, range, top customers, closing report). Realtime for the landing page = polling fallback.

### 6.9 Staff Auth + Agent Money Tools ✅ (Phase 7)

**Staff auth (owner/pegawai) — the proxy's own identity, no Supabase:**
- `sports_users` (email, bcrypt password, role owner|pegawai, is_active) + `sports_sessions` (sha256-hashed 32-byte tokens, 30-day expiry)
- `POST /v1/auth/login` `{tenant_id, email, password}` → `{token, user}`; `POST /v1/auth/logout`; `GET /v1/auth/me`
- **Unified identity middleware**: `X-Session-Token` (staff, role-per-request) OR `X-Client-Token` (client, full access — backward compat for API/agent)
- **Owner gates**: expenses/pricing/series/courtblocks writes + payments delete + customers delete require `owner` role (client tokens also pass)
- Staff identities bypass the feature gate (they're already inside the tenant)

**Agent money tools** (ported from Centrum's Gemini, now on the shared LLM key — no Gemini):
- `query_net_profit`, `query_range_summary` (cash/qris/expenses/net), `top_customers`, `closing_report`, `query_kantin_summary`
- **Per-tenant daily cost cap**: Redis counter, 150 LLM calls/day (Centrum's guard)

**Availability (realtime-lite):** `GET /v1/sports/availability?date=` → busy intervals per facility (bookings + blocks). Landing page polls; WebSocket = future.

**Deferred:** WebSocket realtime; public (no-token) availability; Centrum frontend's Gemini stays until the frontend migrates (the proxy/agent never uses Gemini).

### 6.3 Admin — Requires `Authorization: Bearer ADMIN_API_KEY` (server side only, never from client sites)

- `POST /v1/admin/clients` → `{name, tenant_id, monthly_quota_tokens?}` → generate 32-byte `crypto/rand` hex(64 chars), store `sha256Hex(token)`, return `{id, name, tenant_id, token:"<RAW — shown ONCE>", monthly_quota_tokens}`. Raw token **never stored, never logged**, shown once.
- `PATCH /v1/admin/clients/{id}` → `{monthly_quota_tokens?, status?}` (`active`/`paused`/`blocked`) + `DEL ck:*` evict + quota cache evict.
- `GET /v1/admin/usage?from=&to=&client=` → aggregated rows `[{client_id, name, tenant_id, tokens_in, tokens_out, cost_usd}]` grouped by client, never returns tokens.

### 6.4 `GET /healthz`

`{"status":"ok","version":"dev"}` (version from `git rev-parse --short HEAD` at build time via `-ldflags`). Pings DB (`SELECT 1`) + Redis (`PING`) — if either fails, `500 {"status":"degraded","db":false,"redis":false}`. Distinct from optional `/_readyz` if added.

---

## 7. Config + Security (Checklist — Miss = Fail)

| Var | Required | Default | Notes |
|---|---|---|---|
| `PORT` | No | `8080` | `cmd/server` listens here |
| `DATABASE_URL` | **Yes** | — | `postgres://user:pass@host:5432/db?sslmode=require` — Supabase or Laragon local `postgres://postgres@localhost:5432/agents_proxy` |
| `REDIS_URL` | No | `localhost:6379` for Laragon dev | Prod: `rediss://:pass@host:6379/0` |
| `LLM_BASE_URL` | No | `https://api.deepseek.com` | Provider-swappable |
| `LLM_API_KEY` | **Yes** | — | Secret — never in repo, never logged |
| `LLM_MODEL` | No | `deepseek-chat` (or `mimo` cheap tier yr2) | — |
| `ADMIN_API_KEY` | **Yes** | — | Secret — generate via `openssl rand -hex 32` |
| `MAX_TOKENS_PER_REQUEST` | No | `2048` | — |
| `RATE_LIMIT_PER_MINUTE` | No | `60` | — |
| `ALLOW_ORIGINS` | No | `*` for dev | Prod: `https://client-a.my.id,https://client-b.my.id` (csv) |
| `ENV` | No | `development` | `development`|`production` — controls CORS strictness |

- `.gitignore` ignores `.env`, `bin/`, `*.log`, `.air.toml`.
- Rate limit per `client_token_hash` + per IP for unauth traffic (prevents token brute force).
- CORS: `Allow-Origin` from `ALLOW_ORIGINS` (csv), handle `OPTIONS` preflight, `credentials: false`.
- Validate every input (vertical, kind, page/limit ranges, JSON parse → `400 {"error":"..."}`). Never string-interpolate into SQL.
- SQL: `pgx` **parameterized only** (`$1,$2`). Never `fmt.Sprintf` into SQL.
- Logs: `log/slog` JSON; fields `method, path, status, duration_ms, client_id` (if known); **never** tokens/bodies. `ReplaceAttr` redacts `Authorization`.
- Graceful shutdown: `SIGINT/SIGTERM` → `srv.Shutdown(ctx)` 10s timeout.
- Go 1.27 hardening: `Server{ReadHeaderTimeout: 5*time.Second, ReadTimeout: 10*time.Second, IdleTimeout: 120*time.Second, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}` — leave `CurvePreferences` nil so PQC `X25519MLKEM768` auto-applies. Use `os.Root` for any file access.

---

## 8. Tests (Green or Not Done — `httptest` + `httptest` Server Faking DeepSeek)

- `TestAuth` — unknown token → 401, paused client → 403, missing token → 401.
- `TestQuota` — pre-seed `usage` that exceeds quota → 429 with `resetAt` ISO, `Retry-After`.
- `TestRateLimitRedis` — burst of `RATE_LIMIT_PER_MINUTE + 5` requests → some `429` with `Retry-After` (hits real Redis via `go-redis` against test Redis; or `miniredis` if unavailable — document requirement).
- `TestBusinessTenantIsolation` — client A `POST /v1/fnb/records`, client B `GET /v1/fnb/records` → B gets `[]` / `404` on A's `id`.
- `TestChatHappy` — valid token → provider mock (httptest server) returns `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":20}}`, handler returns `{"reply":"hi"}`, usage row inserted, `tokens_in/out` recorded, Redis quota cache updated.
- `TestChatHappyRedisCached` — second call with same token hits `ck:{hash}` cache (no DB hit — assert via mock count or cache key exists).
- `TestAdmin` — `POST /v1/admin/clients` with correct admin bearer → token returned **once**, stored as hash (`SELECT token_hash FROM clients` shows hash, not raw); wrong admin → 401; `PATCH` to `paused` → subsequent `POST /v1/chat` → 403.

`go test ./... -cover` reported in final report (aim ≥70%).

---

## 9. Docker (Prod Only — Dev is Laragon) ✅

- `Dockerfile` — multi-stage: `golang:1.27-alpine AS build` → `CGO_ENABLED=0 go build -ldflags="-w -s -X main.version=$(git rev-parse --short HEAD)" -o /bin/server ./cmd/server` → `FROM scratch` (or `gcr.io/distroless/static:nonroot`) + CA certs (`apk add --no-cache ca-certificates` in build, copy `/etc/ssl/certs`), `EXPOSE 8080`, `CMD ["/bin/server"]`. Size ~15MB.
- `docker-compose.yml` — `postgres:16-alpine` (healthcheck `pg_isready`) + `redis:8-alpine` (healthcheck `redis-cli ping`) + `app` building from `.` (depends_on `healthy`, runs migrations on start via `migrate.go`, healthcheck `curl -f http://localhost:8080/healthz`). **This file is for Railway/VPS deploys, NOT for local Laragon dev.** Document this.
- `Makefile` — `run` (`go run ./cmd/server`), `build` (`go build`), `test` (`go test ./... -cover`), `vet` (`go vet ./...`), `tidy` (`go mod tidy`), `docker` (`docker build`), `redis-cli` (`redis-cli -h localhost`).

## 10. Local Dev (LARAGON — Not Docker)

Laragon is your daily driver. Document this exact flow in README:

1. Copy `.env.example` → `.env` (`DATABASE_URL=postgres://postgres@localhost:5432/agents_proxy?sslmode=disable` for Laragon Postgres, `REDIS_URL=localhost:6379`, `LLM_API_KEY=...`, `ADMIN_API_KEY=...`).
2. Laragon → Start All → ensure `Postgres` (5432) + `Redis` (6379) show green (Services → check). Create DB `agents_proxy` in HeidiSQL if first run.
3. `cd D:\MyProject\AgentsProxy && go run ./cmd/server` — migrations auto-run on boot, server on `:8080`.
4. Verify: `curl -s http://localhost:8080/healthz` → `{"status":"ok"}`. HeidiSQL → `SELECT * FROM clients;`. `redis-cli KEYS "ck:*" "rl:*" "quota:*"` to see caching live.
5. `go vet ./... && go test ./... -cover` — green before commit.

---

## 11. Verify (Do Before Saying Done — All from `D:\MyProject\AgentsProxy`)

Run in order, fix until green, then report:

1. `go mod tidy && go vet ./... && go test ./... -cover` — show coverage.
2. Laragon Start → `go run ./cmd/server` → `curl -s http://localhost:8080/healthz` (output must include `"status":"ok"` and DB+Redis `true`).
3. Full loop:
   - `POST /v1/admin/clients` with `Authorization: Bearer $ADMIN_API_KEY` → `{id, token}`
   - `POST /v1/chat` with `{"client_token":"<token>","messages":[{"role":"user","content":"halo"}]}` → `{"reply":"..."}`
   - `GET /v1/usage` with `X-Client-Token: <token>` → usage row
   - `POST /v1/fnb/records` with `X-Client-Token` → `201`
   - `GET /v1/fnb/records` with same token → list contains it
   - `GET /v1/fnb/records` with B's token → `[]` / 404 on A's id (tenant isolation)
   - `redis-cli KEYS "rl:*" "ck:*" "quota:*"` — show caching live
4. `docker build -t agents-proxy .` succeeds (size ~15MB).

**Final report:** repo tree, endpoint table, `go test -cover` summary, exact steps a DFD agent follows to onboard a client (`POST /v1/admin/clients` → inject `NEXT_PUBLIC_AGENTS_PROXY_URL` + `NEXT_PUBLIC_AGENTS_PROXY_TOKEN` into site), migrations list, which phase shipped (AI proxy must be Phase 1 green separately), known limitations.

---

## 12. Order Rule — Build Phase 1 (AI Proxy + Admin + Healthz + Middleware + Redis Cache) FIRST and Make It Green (Perf + Security Perfect) Before Phase 2 (Business Monolith). Report Phase 1 Green Separately, Then Phase 2. ✅

## 13. Extensibility Note (Inside README) ✅

Phase 3 reduces the extensibility rule to a one-liner: **add a feature = drop a folder + add one `Register` call**. Examples:

- **Add a shared feature** (e.g. `invoice`): copy `internal/features/shared/menu/` to `internal/features/shared/invoice/`, edit the validator + `Kind`, blank-import from `cmd/server/main.go`. Default set lives in `internal/features/defaults.go`.
- **Add a custom feature** for one tenant: copy `internal/features/clients/_template/` to `internal/features/clients/<tenant>/`, edit `Name`, `Kind`, `VerticalSlugs`, `ValidatePayload`, blank-import from `cmd/server/main.go`. **The isolation test (`feature_test.go`) is required.**
- **Add vertical 13**: append one string to `internal/features/builder.go`'s `AllowedVerticals`. Add a default feature set in `defaults.go` if the vertical should get a different default.

No DDL is required for any of the above unless a custom feature needs its own table (in which case it ships its own migration with RLS — see `clients/_template/README.md`).

Cursor pagination is the rule for every list endpoint going forward (§6.2 + §6.5). New endpoints must not introduce `LIMIT/OFFSET` style pagination. The cursor token is a base64-encoded `(created_at_nano, id)` pair; the +1 over-fetch trick produces `has_more` and the next token without an extra round-trip.

> Monolithic does not mean monolithic DB row: verticals are virtualized via `payload->>kind`. Add vertical 13 = add one string to `allowedVerticals`. All Hostinger static sites share this ONE Go deploy; adding a feature never requires a new DB or new infra. Redis makes it stay fast.

---

## 14. Critical Files to Modify (Inventory)

- **Create:** `PRD.md` (this file), `ARCHITECTURE.md`, `STANDARDS.md`, `README.md`, `.env.example`, `.gitignore`, `Makefile`, `Dockerfile`, `docker-compose.yml`, `go.mod`, `migrations/001-003.sql`, `cmd/server/main.go`, `internal/**/*` (see §2 tree).
- **Do NOT touch:** `D:\MyProject\Langchain`, `D:\MyProject\MainTemplates`, `D:\MyProject\DFDWebsite` — this repo is self-contained.

---

## 15. Non-Functional Requirements (The "Why" Behind the Latest Stack)

- **Performance:** Go 1.27 Green Tea GC + Swiss Tables + `pgxpool` + Redis `io-threads 8` = one binary serves all clients on <50MB. Static Hostinger frontends + pooled Postgres + hot Redis = zero cold DB on hot path.
- **Security:** `subtle.ConstantTimeCompare`, parameterized `pgx`, `STABLE SECURITY DEFINER` RLS, `slog` redaction, `ADMIN_API_KEY` never logged, `os.Root` for any file access.
- **Scalability:** Vertical 13 = one string. Tenant 100 = same DB. Horizontal scaling = second container pointing at same Redis/Postgres.
- **Maintainability:** stdlib `net/http` + `chi` + `pgx` + `go-redis` are the most maintained Go libs. Tests colocated `*_test.go`, `go vet` + `go fmt` on every commit.

