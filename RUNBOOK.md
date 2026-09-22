# RUNBOOK — D:\MyProject\AgentsProxy

> Operations guide for the deployed agents-proxy. Pair with `PRD.md`, `DONE.md`, `RUNBOOK.md` lives in the repo so any operator has context.

---

## 1. Health & Liveness

```bash
curl -s https://<host>/healthz
# {"status":"ok","version":"<sha>","db":true,"redis":true}
```

If `db` or `redis` is `false`, see "Service Down" below.

UptimeRobot should ping `GET /healthz` every 5 min and alert on 5xx.

---

## 2. Logs (slog JSON → stdout)

All logs are JSON on stdout. In production (Railway), `railway logs -f`.

Quick filters:

```bash
# All errors
railway logs -f | grep '"level":"ERROR"'

# All panics
railway logs -f | grep '"msg":"panic recovered"'

# Per-client traffic (uses client_id in attrs)
railway logs -f | jq 'select(.client_id=="<uuid>")'

# Per-request via request_id (returned in X-Request-ID)
railway logs -f | jq 'select(.request_id=="<id>")'
```

---

## 3. Common Incidents

### 3.1 `429` Spike

- **What it means:** clients hitting `quota` (monthly) or `ratelimit` (per-minute).
- **Diagnose:**
  ```bash
  # Top clients by 429s in last 1h
  railway logs --since 1h | grep '"status":429' | jq -r .client_id | sort | uniq -c | sort -rn | head
  ```
- **Fix:**
  - Quota exhaustion: `POST /v1/admin/clients` then `PATCH /v1/admin/clients/{id} {"monthly_quota_tokens": <new>}` to raise the cap. Or wait until the 1st of next month.
  - Rate limit: a noisy neighbour or a buggy client loop. Check `redis-cli` for `rl:*` keys:
    ```bash
    redis-cli -h <host> KEYS "rl:*" | head
    redis-cli -h <host> GET "rl:<clientID>:<YYYYMMDDHHmm>"
    ```
    PATCH the client to `status="paused"` while you sort it out.

### 3.2 LLM Provider Errors (502 from /v1/chat)

- **What it means:** the upstream (DeepSeek / mimo / etc.) returned non-2xx or timed out.
- **Diagnose:**
  ```bash
  railway logs -f | grep '"msg":"listen"'  # confirm LLM_BASE_URL
  railway logs -f | grep '"status":502'    # count of 502s
  ```
- **Fix:**
  - Provider outage: wait + `POST /v1/admin/clients` is unaffected. Chat returns 502 to the client widget; user sees a graceful error.
  - Bad `LLM_MODEL`: update `LLM_MODEL` env, restart container.
  - Bad `LLM_BASE_URL`: same — fix env, restart.

### 3.3 Postgres Down

- `GET /healthz` returns 500 with `db:false`.
- Diagnose from a shell with `DATABASE_URL`:
  ```bash
  psql "$DATABASE_URL" -c 'SELECT 1'
  ```
- Fix: see Supabase status page (prod) or `laragon start` (dev).

### 3.4 Redis Down

- `GET /healthz` returns 500 with `redis:false`.
- Cache misses fall back to DB. `rate-limit` middleware fails-open (allows the request) — this is by design, see `middleware/ratelimit.go`. So Redis being down means loss of caching only, not a hard outage.
- Fix: restart Redis. Verify with `redis-cli -h <host> ping`.

### 3.5 Sentry Not Receiving Events

- Check `SENTRY_DSN` is set in env (`railway variables | grep SENTRY`).
- Check `slog` output for `"msg":"sentry initialised"` on boot.
- Check sample rate: `TRACES_SAMPLE_RATE=0.1` means 10% of transactions — bump to `1.0` temporarily to confirm pipeline.

---

## 4. Roll-back

```bash
# 1. Identify the last good commit
git log --oneline -20

# 2. Revert (creates a new commit; safe)
git revert <bad-sha>
git push origin main

# 3. Wait for CI to pass; deploy auto-runs on push to main.
#    If you need an immediate hotfix without CI, deploy a known-good tag:
railway up --detach  # uses current branch
```

For data incidents: restore from `ops/backup.sh` output:
```bash
gunzip -c backups/agents_proxy-2026-08-31T17-00-00Z.sql.gz | psql "$DATABASE_ADMIN_URL"
```

---

## 1.5 Environment variables (full table)

| Env | Default | Notes |
|---|---|---|
| `PORT` | 8080 | |
| `ENV` | development | `development` / `production` |
| `DATABASE_URL` | required | runtime pool — connect as `app_runtime` in production (Supabase Supavisor 6543) |
| `DATABASE_ADMIN_URL` | empty | migrations + admin endpoints — connect as superuser (Supabase 5432 direct) |
| `REDIS_URL` | localhost:6379 | production: `rediss://...` for TLS |
| `LLM_BASE_URL` | https://api.deepseek.com | |
| `LLM_API_KEY` | required | |
| `LLM_MODEL` | deepseek-chat | |
| `ADMIN_API_KEY` | required | 32+ bytes; the only thing that can call `/v1/admin/*` |
| `MAX_TOKENS_PER_REQUEST` | 2048 | |
| `RATE_LIMIT_PER_MINUTE` | 60 | per `client_id` post-auth; per `ip` pre-auth (`/v1/chat`) |
| `ALLOW_ORIGINS` | * | CSV; production: explicit origin list |
| `SENTRY_DSN` | empty | empty = no-op (Sentry disabled) |
| `SENTRY_ENV` | development | |
| `SENTRY_RELEASE` | empty | default: `-ldflags="-X main.Version=..."` |
| `TRACES_SAMPLE_RATE` | 0.1 | 0.0..1.0 |
| `DB_POOL_MIN` | 2 | runtime pool minimum connections |
| `DB_POOL_MAX` | 20 | runtime pool maximum connections |
| `DB_USE_SIMPLE_PROTOCOL` | false | **set to `true` when connecting via Supavisor 6543** |
| `TELEGRAM_VAULT_KEY` | empty | 64-hex master key; empty = bots disabled, REST unaffected. Generate: `openssl rand -hex 32` |
| `PROXY_PUBLIC_URL` | empty | public base URL for Telegram webhooks; empty + `TELEGRAM_POLL_DEV=1` = dev polling |
| `TELEGRAM_POLL_DEV` | false | per-bot getUpdates polling fallback (dev only; prod uses webhooks) |

## 1.5.5 Telegram multi-bot ops

- **Register a bot:** `POST /v1/sports/telegram/bots {"name":"Owner","role":"owner","token":"...","allow_chat_ids":[...]}` (owner session). Token validated via `getMe`, encrypted at rest, webhook registered automatically.
- **Revoke:** `DELETE /v1/sports/telegram/bots/{id}` (webhook dropped) or `PATCH ... {"status":"disabled"}`. BotFather revocation is separate.
- **Find a user's chat_id:** message the bot once; check logs; or use @userinfobot. Owner/pegawai bots refuse non-allowlisted chats.
- **Reminders:** stored in `agent_reminders` with owning `bot_id`; scheduler polls every 60s and sends via that bot. Undeliverable rows flip to `dead` (bad chat after parse fail, 5 failed attempts).
- **Staff payments are a ledger** (`agent_staff_payments`) — no money movement. Real payouts = future phase.

## 1.6 Feature toggle (instant or ≤15 min)

`PATCH /v1/admin/clients/{id} {"features":["records","menu","booking"]}` — the cache entry is evicted on the way out, so the next request sees the new feature set. If Redis is down, the cached entry lives up to 15 minutes (the cache TTL); use `redis-cli DEL ck:<sha256>` to force the eviction.

## 1.7 Escape-hatch decision rule

A request is **in-request** when:
- it finishes in <300ms, AND
- it touches no third-party API that needs a secret, AND
- it does not need to write to a non-proxied storage (e.g. an external billing API).

Otherwise: **Edge Function** (DFD Supabase Edge Functions or a Hostinger serverless endpoint). The proxy stays the canonical path for LLM, cache, and tenant CRUD; the Edge Function handles the side trip.

---

## 5. Client Onboarding (DFD agent flow)

```bash
ADMIN="Authorization: Bearer $ADMIN_API_KEY"

# 1. Create client (one-time per client)
curl -s -X POST https://<host>/v1/admin/clients \
  -H "$ADMIN" -H "Content-Type: application/json" \
  -d '{"name":"warung-sari","tenant_id":"warung-sari","monthly_quota_tokens":500000}'
# → returns {id, name, tenant_id, token, monthly_quota_tokens}
#    ↑ the `token` is shown ONCE. Save it now.

# 2. Inject into the Hostinger static site (next.config.js or env):
#    NEXT_PUBLIC_AGENTS_PROXY_URL=https://<host>
#    NEXT_PUBLIC_AGENTS_PROXY_TOKEN=<token from step 1>
#    NEXT_PUBLIC_AGENTS_PROXY_TENANT=warung-sari

# 3. Verify
curl -s https://<host>/v1/fnb/records -H "X-Client-Token: <token>"
# → 200, items: []  (empty until first POST)

# 4. Suspend / resume
curl -X PATCH https://<host>/v1/admin/clients/<id> \
  -H "$ADMIN" -H "Content-Type: application/json" \
  -d '{"status":"paused"}'
```

---

## 6. Schema Migrations

Migrations run automatically on boot via `internal/db/migrate.go`. Each migration is wrapped in a `pg_advisory_xact_lock(734561)` to keep parallel boots from racing on `CREATE EXTENSION pgcrypto`.

To add a migration: drop a new file into `internal/db/migrations/00X_name.sql`; sort order is lexical. **Never edit an applied migration** — add a new one that does the delta.

**Phase 4 note:** migrations `007`–`010` (sports vertical) create custom tables with RLS. Migration `008` runs `CREATE EXTENSION btree_gist` (needs superuser) — migrations run on the admin pool, so this is fine. The booking exclusion constraint (`tsrange`, NOT `tstzrange` — see ARCHITECTURE §11.3) prevents double-bookings at the DB level; a concurrent double-book surfaces as HTTP `409 slot already booked`.

---

## 7. Performance & Capacity

- **Latency targets:** p95 `<100ms` on cache hit, p99 `<500ms` on LLM call (provider-dominated).
- **DB pool:** `MinConns 2, MaxConns 20` per replica. If you see `conn busy` in logs, scale horizontally.
- **Redis hot keys:** `ck:{hash}` (15m TTL) + `rl:{id}:{min}` (60s TTL) + `quota:{id}:{month}` (30s TTL). On Redis 8 with `allkeys-lru`, 75% `maxmemory` is the recommended ceiling.

---

## 8. Security Audit Checklist

- [x] `subtle.ConstantTimeCompare` on token + admin key (auth/tokens.go)
- [x] `pgx` parameterized (`$1,$2`) — no `fmt.Sprintf` into SQL anywhere
- [x] RLS on `tenant_records` with `FORCE ROW LEVEL SECURITY` + RESTRICTIVE-style predicate (migration 002)
- [x] slog redaction: `Authorization` + any `*password*` / `*secret*` keys → `[REDACTED]`
- [x] Sentry BeforeSend: same redaction
- [x] Rate limit + quota
- [x] `Server{ReadHeaderTimeout: 5s, ReadTimeout: 10s, IdleTimeout: 120s}`
- [x] `govulncheck` in CI

---

## 9. When You Need to Change Things

| You want to… | Touch |
|---|---|
| Add a new vertical | `internal/api/business.go` — append one string to `allowedVerticals` |
| Add a new business field | nothing — it's all in `payload` jsonb |
| Add a new admin endpoint | `internal/api/admin.go` |
| Change rate limit | `RATE_LIMIT_PER_MINUTE` env |
| Add a Sentry env var | `internal/config/config.go` + `internal/sentry/sentry.go` |
| Add a migration | new file in `internal/db/migrations/`, never edit applied ones |
| Change the LLM provider | `LLM_BASE_URL` + `LLM_API_KEY` + `LLM_MODEL` env |
