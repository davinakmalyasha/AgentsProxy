# STANDARDS — D:\MyProject\AgentsProxy

> Golden standard for this Go repo. Every change must satisfy ALL sections. No PR merges otherwise.
> This is the checklist other agents (and you in 6 months) run before saying green.

---

## A. Stack (Mandatory — Pin Exact Minors)

- **Go:** `1.27` (or latest `go version` shows — pin it, do not float `go 1.x`). Build with `-pgo=auto`.
- **Router:** `net/http` stdlib (`"GET /v1/chat"`, `r.PathValue("id")`) + `chi/v5` only when nested groups/scoped middleware is needed. No Gin/Echo/Fiber.
- **DB driver:** `jackc/pgx/v5` + `pgxpool` (binary protocol, prepared statements by default). No `lib/pq`, no GORM.
- **Cache:** `go-redis/v9` (official). No `rueidis` without justification.
- **Logging:** `log/slog` JSON handler — no `logrus`, no `zap`.
- **File access:** `os.Root` — never `filepath.Join + Clean + prefix`.
- **DB:** Supabase Postgres 17 (one shared project) — never one DB per client.
- **Cache server:** Redis 8.10.1 — `io-threads 8`, `allkeys-lru` or `volatile-lru` with `maxmemory 75%`.

---

## B. Local Quality Gates (Must Pass Before Commit — Laragon Dev)

Run from `D:\MyProject\AgentsProxy`:

```bash
go mod tidy
go vet ./...
go fmt ./...
go test ./... -cover
go build ./...
```

All green, **zero warnings**, `vet` clean, `fmt` applied, `build` passes, `test -cover ≥70%`. No `//go:vet` suppression without a comment explaining the false positive.

---

## C. Testing (Vitest Equivalent for Go — `httptest` + `httptest` Server Faking DeepSeek)

- **Framework:** `net/http/httptest` + optional `testify/assert` (thin). No `gomock` unless a clear interface mock is needed.
- **Coverage:** `≥70%` lines, enforced in `go test -cover` (no threshold fail, but document the number in the PR).
- **Required tests (miss = fail):**

| Test | Assertion |
|---|---|
| `TestAuth` | Unknown token → 401, paused client → 403, missing token → 401 |
| `TestQuota` | Pre-seed `usage` that exceeds quota → 429 with `resetAt` ISO + `Retry-After` |
| `TestRateLimitRedis` | Burst of `RATE_LIMIT_PER_MINUTE + 5` → some `429` with `Retry-After` (hits real Redis or `miniredis`) |
| `TestBusinessTenantIsolation` | Client A `POST /v1/fnb/records`, client B `GET` → B gets `[]` / `404` on A's id |
| `TestChatHappyRedisCached` | Valid token → provider mock reply, usage row inserted, `ck:{hash}` cached (second call hits cache) |
| `TestAdmin` | `POST /v1/admin/clients` returns token **once**, stored as hash; wrong admin → 401; `PATCH` to `paused` → chat 403 |

- **Mock provider:** `httptest.NewServer` standing in for DeepSeek (`/chat/completions` returns `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":20}}`).
- **Isolation:** Each test `TRUNCATE clients, usage, tenant_records CASCADE` in its own `BEGIN/COMMIT` or fresh `httptest` DB transaction.

---

## D. Security (Miss = Fail)

| Rule | How | Why |
|---|---|---|
| **Zero secrets in repo** | `.env.example` documents every var; `.gitignore` ignores `.env`, `bin/`, `*.log` | Repo is private but CI runners + clones leak |
| **Token storage** | `sha256Hex(token)` in `clients.token_hash` only; raw token shown **once** on `POST /v1/admin/clients`, never stored, never logged | DB leak = still useless |
| **Constant-time compare** | `subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1` | Timing attack on hash |
| **SQL** | `pgx` parameterized `$1,$2` ONLY — never `fmt.Sprintf` into SQL | Injection |
| **RLS** | `ALTER TABLE tenant_records ENABLE ROW LEVEL SECURITY` + `RESTRICTIVE` policy + `SET LOCAL app.tenant_id` before every DML | App-layer bug cannot leak tenants |
| **Grants** | Revoke `anon`/`authenticated` auto-grants to needed ops only, `TO authenticated` in policies | Policy ≠ grant |
| **File access** | `os.Root` | Symlink escape (CVE-2026-39822) |
| **Headers** | `Server{ReadHeaderTimeout:5s, ReadTimeout:10s, IdleTimeout:120s, TLSConfig:{MinVersion:TLS12}}` — leave `CurvePreferences` nil (PQC auto) | Slowloris + PQC |
| **Headers** | `CSP` + `security headers` middleware, `CORS` from `ALLOW_ORIGINS` csv, credentials off | XSS + CORS |
| **Rate limit** | Per `token_hash` (authenticated) + per IP (unauth, brute-force guard) via Redis `INCR+EXPIRE` 60s | Burst abuse |
| **Validation** | `vertical ∈ {12 allowed}` (400 if unknown), `kind` non-empty, `page/limit` ranges, JSON parse → `400 {"error":"..."}` | Input fuzz |
| **Logs** | `slog` `ReplaceAttr` redacts `Authorization`, never log tokens/bodies/LLM prompt | PII + secret leak |
| **Dependencies** | `go vet` + `govulncheck ./...` in CI | Known CVEs |

---

## E. Performance (Miss = Fail)

| Rule | Target |
|---|---|
| **Static vs server** | Client sites are `output: "export"` static on Hostinger — zero Node per-client. Proxy is the ONLY server. |
| **Latency** | p95 `<100ms` on cache hit, p99 `<500ms` on LLM call (provider dominates). Measure via `slog duration_ms` |
| **DB** | `pgxpool` Min 2 Max 20, `MaxConnLifetime 1h`, Supavisor `6543` TX mode for runtime (serverless-friendly), `5432` session for migrations. `EXPLAIN ANALYZE` on `usage` SUM query. |
| **Redis** | `io-threads 8`, `volatile-lru`/`allkeys-lru` with `maxmemory 75%`, `HGETEX` 1 RTT. Do not use DB-per-tenant (100 tenants = OOM, see ARCHITECTURE). |
| **Build** | `-pgo=auto`, Swiss Tables + Green Tea GC are defaults — verify with `GOMEMLIMIT` under load. |
| **Lighthouse** | Not applicable (API only); client templates have separate budget: perf ≥90, a11y ≥95. |

---

## F. API Contract & Validation

- **Auth:** `X-Client-Token: <token>` for business/chat (do not confuse with admin `Authorization: Bearer`). Validate token required → 401.
- **Vertical:** `vertical ∈ {fnb,salon,laundry,klinik,gym,bimbel,bengkel,beauty,travel,property,katering,photo}` — 400 if unknown.
- **Pagination:** `limit` default 20 max 100, `page` ≥1.
- **Errors:** `400 {"error":"..."}`, `401/403` for auth, `429 {"error":"quota exceeded","resetAt":"..."} + Retry-After`, `500 {"error":"internal"}` (sanitize provider errors).

---

## G. CI (GitHub Actions — Add When Repo Is Pushed) ✅

- `ci.yml` — push/PR: `go mod tidy --check` → `go vet` → `go test -cover` → `go build` → `govulncheck`. ✅
- `security.yml` — weekly + push: CodeQL, `govulncheck`, secret scan (`gitleaks`), `go mod tidy --check`. ✅
- `release.yml` — tag push: `go build` with `-ldflags="-X main.version=$(git rev-parse --short HEAD)"` → push image to `ghcr.io`. ✅

---

## H. Post-Deploy Operations (Required — The Part People Forget) ✅

- **`GET /healthz`** → `{"status":"ok","version":"dev"}` (pings `SELECT 1` + Redis `PING`; 500 if either down). Distinct from optional `/_readyz`. ✅
- **Uptime:** `ops/uptime.md` — UptimeRobot `https://<host>/healthz` every 5 min. ✅ (documented in RUNBOOK.md)
- **Backup:** `ops/backup.sh` — `pg_dump` (Supabase `DATABASE_URL` with `?sslmode=require`) + `psql` restore doc, weekly via Hostinger cron or `pg_cron`. ✅
- **Sentry (env-gated):** `SENTRY_DSN` via env, init in `cmd/server` when set, `NO-OP` when absent (site works without it). ✅ (full setup: release, traces, profiles, panic capture, slog bridge, PII redaction)
- **`RUNBOOK.md`:** how to monitor (`slog` JSON query), back up, roll back (`git revert` + redeploy), respond to 429 spikes. ✅

---

## I. Docs

- **README.md:** Title + summary, Stack table, Quick start (Laragon: copy `.env.example` → Start All → `go run ./cmd/server` → `curl /healthz`), Env table, Endpoints table (method+path+auth+examples), How to add vertical 13 / custom `kind`, Deploy (Railway `railway up` or VPS `docker compose up -d`), Maintaining the monolith (file to touch per change), Known gaps.
- **.env.example:** Documents EVERY var with a comment block (see PRD §7).
- **PRD.md + ARCHITECTURE.md:** This repo's decision record — never delete, update with each architectural change.

---

## J. Non-Functional Requirements (The "Why")

- **Performance:** Go 1.27 Green Tea + Swiss Tables + `pgxpool` + Redis `io-threads` = one binary <50MB serves all clients on Hostinger/VPS. Static frontends + pooled Postgres + hot Redis = zero cold DB on hot path.
- **Security:** `subtle.ConstantTimeCompare`, parameterized `pgx`, `STABLE SECURITY DEFINER` RLS, `slog` redaction, single `ADMIN_API_KEY` never logged, `os.Root`.
- **Scalability:** Vertical 13 = one string in `allowedVerticals`. Tenant 100 = same DB. Horizontal = second container pointing at same Redis/Postgres.
- **Maintainability:** `net/http` stdlib + `chi` + `pgx` + `go-redis` are the most maintained Go libs. Tests colocated, `go vet`+`go fmt` on every commit. PRD is the spec, ARCHITECTURE is the why.

