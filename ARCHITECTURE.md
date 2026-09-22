# ARCHITECTURE — D:\MyProject\AgentsProxy

> Why the latest versions were chosen and what they actually deliver vs the old stack.
> This is the decision record other agents (and you in 6 months) read to understand *why*.

---

## 1. Stack Snapshot (Pinned at Build Time)

| Layer | Choice | Version | Why Not the Alternative |
|---|---|---|---|
| **Language** | Go | `1.27` (latest stable, pin minor) | Node: already powers DFD site, but gateway wants binary + GC control. Python: great for agents, not for a 15MB proxy binary. |
| **Router** | `net/http` stdlib + `chi/v5` thin wrapper | `chi v5.0.11+` (1k lines, 0 deps) | Pure stdlib: verbose route groups. Gin/Fiber: custom context, fasthttp incompat, reflection overhead. Chi = stdlib `http.Handler` + clean `r.Route("/v1", func(r chi.Router){ r.Use(auth); })` |
| **DB driver** | `jackc/pgx/v5` + `pgxpool` | `v5.6+` | `database/sql` + `lib/pq` = text protocol, slower; GORM = hidden SQL, harder to audit for injection. pgx = binary protocol + prepared statements by default |
| **Cache** | `go-redis/v9` | `v9.5+` | `rueidis` is faster (auto-pipelining) but `go-redis` is official Redis client, docs/stable. Either is defensible — pin one. |
| **DB** | Supabase Postgres 17 (managed) / local Postgres 16 via Laragon | PG17 | PG15 EOL mid-2026. PG17: faster `VACUUM`, `MERGE`, `pg_stat_io`, 700+ fixes |
| **Cache server** | Redis 8.10.1 | `8.10` | Redis 7 would work; 8 brings 1-RTT atomics + io-threads + compact hashes — free perf |

---

## 2. Go 1.27 — What New Actually Matters Here

### 2.1 `net/http` Enhanced ServeMux (Go 1.22+, foundation for 1.27) — Replaces `chi` for Flat Routes

**Old:**
```go
http.HandleFunc("/v1/agents/", func(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet { http.Error(w, "method not allowed", 405); return }
    id := strings.TrimPrefix(r.URL.Path, "/v1/agents/")
    id = strings.Split(id, "/")[0] // manual, no 405 Allow header
})
```

**New (Go 1.22+):**
```go
mux := http.NewServeMux()
mux.HandleFunc("GET /v1/agents/{id}", getAgent)      // method + path param in pattern
mux.HandleFunc("POST /v1/agents", createAgent)
mux.HandleFunc("/files/{path...}", serveFile)        // ... wildcard
mux.HandleFunc("/posts/{$}", listPosts)              // {$} exact trailing slash
id := r.PathValue("id")                               // new API
```

Unmatched method → `405 Method Not Allowed` with correct `Allow` header automatically. Precedence = most specific wins, order-independent. Both pattern + incoming path are unescaped segment-by-segment (`/%2F` = 2 segments, fixes old #21955).

**For this proxy:** flat routes (`GET /healthz`, `POST /v1/chat`, `/v1/{vertical}/records`) are expressible in pure stdlib. **Keep `chi` only if you need nested groups with scoped middleware** (`/api` auth only, `/admin` extra): `r.Route("/v1", func(r chi.Router){ r.Use(auth); r.Post("/chat", ...) })`. `chi` is `http.Handler`-compatible, so you can mix: `chi` front, stdlib handlers inside.

Go 1.25 added `http.NewCrossOriginProtection().Handler(mux)` — stdlib CSRF via `Sec-Fetch-Site` + `Origin` fallback. Use for browser-facing admin if added; for this API-only proxy, CORS middleware is sufficient.

### 2.2 `log/slog` (Go 1.21+, mature in 1.27) — Replaces `logrus`/`zap`

```go
h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
    Level: slog.LevelInfo, AddSource: true,
    ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
        if a.Key == "Authorization" { return slog.String("Authorization", "[REDACTED]") }
        return a
    },
})
slog.SetDefault(slog.New(h))
slog.Info("request", "request_id", id, "duration_ms", dur, "client_id", cid)
```

Unstructured `log.Printf("user=%d")` is unparsable in Loki/ELK. `slog` JSON is indexed, filterable by `level`/`request_id`/`trace_id`. Benchmarks: ~2x faster than `logrus`, slightly slower than `zap` but zero dep and acceptable at 25k QPS proxy.

### 2.3 Security Additions

- **`os.Root` (Go 1.24+):** `root, _ := os.OpenRoot("/safe-dir"); root.Open("data.txt")` — confined, symlink escape blocked. Use for any file access (migrations embed is better, but if you read plugin configs, use this — replaces `filepath.Join + Clean + prefix` which had CVE-2026-39822).
- **`crypto/tls` PQC default (Go 1.24+):** `X25519MLKEM768` hybrid ML-KEM-768 enabled when `CurvePreferences` is nil. **Never set `CipherSuites`/`CurvePreferences` manually** — it preserves future defaults. Add `ReadHeaderTimeout: 5*time.Second`.
- **FIPS 140-3 module (Go 1.24+):** `GOFIPS140=v1.0.0` or `GODEBUG=fips140=only` if a government client requires it — no code change.

### 2.4 Performance Additions (Free)

- **1.24 Swiss Tables `map`** (default, `GOEXPERIMENT=noswissmap` to disable): -2-3% CPU, critical for `map[string]*Upstream` cache.
- **1.26 Green Tea GC (default, opt-out `GOEXPERIMENT=nogreenteagc` until 1.27):** -10-40% GC overhead via locality + AVX512 scanning. Monitor with `GOMEMLIMIT` under high memory stress.
- **1.25 Container-aware `GOMAXPROCS`:** runtime queries cgroup CPU limit live — replaces `automaxprocs` sidecar on Railway/K8s.
- **Build with `-pgo=auto`** (Go 1.23+, overhead now single-digit%) + `go:embed` for migrations.

### 2.5 Minor (Use Selectively)

- **Iterators `iter` (Go 1.23):** `for a := range client.FetchAgents { }` for upstream pagination — not on hot path.
- **`unique` package:** interning repeated strings (`tenant_id`) — 6x memory save at 10k dup strings.
- **`encoding/json/v2` (experimental, Go 1.25+):** prepare abstraction to swap when stable; faster decode for provider JSON.

**Do NOT build on experimental `simd` (`GOEXPERIMENT=simd`)** — note for future vectorized header scanning.

---

## 3. Redis 8.10 — What New Actually Matters Here

### 3.1 I/O Threading (8.0+, opt-in `io-threads 8`, `io-threads-do-reads yes`)

Main thread still executes, I/O threads handle read/parse/write. Measured **+37-112% throughput** (8 threads) vs 7.2; `MGET` pipelined `+68%`, `HGETALL` `+25%`, `XREADGROUP` `+83%`. Your proxy is **90% GET** (token cache, quota, rate-limit), the direct beneficiary. Set `io-threads = physical cores (max 8)`; >8 adds contention.

### 3.2 Atomic Helpers (8.0–8.4) — 2 RTT → 1 RTT

| Old | New | Use Here |
|---|---|---|
| `GET` + `EXPIRE` (2 RTT, race) | `HGETEX key EX 60 FIELDS 1 field` (1 RTT, atomic) | Quota cache refresh: `HGETEX quota:{id} EX 60 FIELDS 1 tokens` |
| `MSET` + `EXPIRE` (2 RTT) | `MSETEX numkeys k v [NX][EX]` (1 RTT) | Batch-warm `ck:{hash}` after deploy |
| `GET` + `DEL` with check | `DELEX key IFEQ value` + `DIGEST` | Idempotency lock release without Lua |
| Key-explosion per window | `HEXPIRE` per-field TTL on a single hash | Per-second quota windows as hash fields (future) |
| `HIMPORT` (8.10) | Compact hash schema-shared | `quota:{id}` with identical fields across millions of keys — replication compression |

**Proxy pattern:**
```redis
# Old (2 RTT)
GET ck:abc
EXPIRE ck:abc 900
# New (1 RTT)
HGETEX ck:abc EX 900 FIELDS 1 data
```

### 3.3 Rate Limiting — Redis Token Bucket (Best Practice for This Proxy)

**Never** `INCR` then non-atomic `EXPIRE` — that allows `limit*2` burst at `00:59–01:01`. Use single Lua `EVALSHA` ( `<1ms, 50k evals/s`):

```lua
-- token-bucket.lua — args: capacity, refillRate, nowSec
local data = redis.call('HMGET', KEYS[1], 'tokens', 'last_refill')
local tokens = tonumber(data[1]) or tonumber(ARGV[1]) -- capacity on first
local last = tonumber(data[2]) or tonumber(ARGV[3])
local elapsed = tonumber(ARGV[3]) - last
tokens = math.min(tonumber(ARGV[1]), tokens + elapsed * tonumber(ARGV[2]))
local allowed = tokens >= 1
if allowed then tokens = tokens - 1 end
redis.call('HMSET', KEYS[1], 'tokens', tokens, 'last_refill', ARGV[3])
redis.call('PEXPIRE', KEYS[1], math.ceil(tonumber(ARGV[1]) / tonumber(ARGV[2]) * 1000) + 1000)
return {allowed and 1 or 0, tokens, math.ceil((1 - tokens) / tonumber(ARGV[2]))}
```

Hybrid with **Sliding Window Counter** (2 `STRING` keys weighted) for strict RPM. Token bucket = burst+avg, counter = hard RPM.

### 3.4 Other 8.10 Wins

- **`volatile-lrm`/`allkeys-lrm`** eviction (least-recently-modified, better than LRU for quota keys).
- **Hot-key detection** + `CLUSTER SLOT-STATS` — detect a single tenant hammering `quota:{id}`.
- **Reduced `HASH` memory** + `kvobj` single alloc (`-25%` short strings) — more `ck:*` keys fit in same RAM.

### 3.5 Redis TLS + ACL (Security)

TLS (`tls-port`, `tls-cert-file`, `tls-auth-clients`, `rediss://`) works with `io-threads` since 8.0, ~15-30% throughput cost — co-locate same AZ. ACL: category `@read` now includes `JSON.GET`, `+@read` audit via `ACL DRYRUN`.

Best practice for this proxy (three least-privilege users):
```conf
ACL SETUSER proxy_cache on >$STRONG ~cache:* ~ck:* +@read +mget +get +exists +ttl resetchannels &*  # EX: HGETEX
ACL SETUSER proxy_quota on >$STRONG ~quota:* ~rl:* ~ck:* +@write +@read +eval +evalsha +incr +expire +pexpire +hexpire +hgetex +hsetex resetchannels &*
ACL SETUSER proxy_invalidator on >$STRONG ~cache:* ~ck:* +del +unlink +scan resetchannels &*
```

---

## 4. Supabase Postgres 17 — One DB for 100 Tenants

### 4.1 Why Shared Schema + RLS Beats DB-per-Tenant

| Model | Cost/Tenant | Migration | Pooling | Limit |
|---|---|---|---|---|
| DB-per-tenant | $25+/mo | x100 | 100x `max_connections` exhaust (2-3MB/idle backend) | 100 tenants = OOM |
| Schema-per-tenant | $5-15/mo | x100 | Complex | Hard |
| **Shared schema + `tenant_id` + RLS** | **Pennies** | **x1** | **1 Supavisor pool multiplexes 10k → 20 backends** | **Sweet spot for 90% SaaS** |

### 4.2 RLS Rules (Miss One = Leak)

1. **Enable on every public table:** `ALTER TABLE ... ENABLE ROW LEVEL SECURITY;` + `FORCE` optional. CI check: `SELECT * FROM pg_tables WHERE rowsecurity = false` must be 0. Policy without `ENABLE` = open.
2. **Fix grants:** legacy `anon`/`authenticated` auto-granted `select/insert/update/delete` — revoke to needed ops only.
3. **One policy per operation** (`FOR SELECT/INSERT/UPDATE/DELETE`, no `FOR ALL` in prod) + `TO authenticated`. `USING` = visible rows, `WITH CHECK` = writable rows. `UPDATE` needs both.
4. **`AS RESTRICTIVE` for tenant boundary:** `CREATE POLICY tenant_boundary ... AS RESTRICTIVE USING (tenant_id IN ...)` — restrictive combines with `AND`, permissive with `OR`. Prevents a later permissive policy leaking via union.
5. **DRY via `SECURITY DEFINER STABLE`:**
```sql
CREATE OR REPLACE FUNCTION private.get_user_tenant_ids() RETURNS SETOF uuid
LANGUAGE sql STABLE SECURITY DEFINER SET search_path='' AS $$
  SELECT tenant_id FROM public.memberships WHERE user_id = (select auth.uid());
$$;
-- then: USING (tenant_id IN (SELECT private.get_user_tenant_ids()))
-- search_path='' + schema-qualified names prevents hijack; owner postgres bypasses RLS recursion
```
6. **Performance: `(select auth.uid())` — wrap in subquery:** bare `auth.uid()` = per-row eval; wrapped = `InitPlan` cached once per statement. **178,000ms → 12ms** benchmark.
7. **Denormalize `tenant_id`:** even child tables must have own `tenant_id` to avoid JOIN in policy.
8. **Indexes mandatory:** without them naive RLS = **10-20x slowdown** (2.1ms→18.4ms). With `(tenant_id, kind)` + `STABLE` definer + `(select ...)` = +50-80% overhead (~3.8ms) — acceptable.

### 4.3 Pooling — `pgxpool` + Supavisor Compose

They compose, not compete. Use **both**:
- `pgxpool` (Go, in-process `puddle`): `MinConns 2 MaxConns 20`, `MaxConnLifetime 1h`, `HealthCheck 1m`. Bounds per replica, saves TLS/auth per query.
- **Supavisor** (Supabase shared pooler, Elixir/BEAM): `500k conns/node`, per-tenant isolation, auto `SELECT`→replica.
  - **Transaction mode `6543`** (IPv4+IPv6) → use for serverless/edge/Vercel — **`BEGIN→COMMIT`** only. No `LISTEN`, no session `pg_advisory_lock` (use `pg_advisory_xact_lock`), no `SET` (use `SET LOCAL`), no `TEMP TABLE` across tx, `prepare:false`.
  - **Session mode `5432`** → use for migrations, `pg_dump`, `LISTEN`.
  - Direct `db.<ref>.supabase.co:5432` → IPv6 only, full parity.
- For Vercel: `connection_limit=1` per function. Keep `<40%` of `max_connections` if using PostgREST/Auth.

### 4.4 What Else Is New (Use Later)

- **Edge Functions** Deno 2.1 (97% faster cold start via `waitUntil`).
- **`pg_cron`** (1s granularity) + **`pgmq`** durable queues + `pg_net` async HTTP — compose for embedding pipelines, not needed here but available.
- **Storage** 14.8x faster deep pages (60M rows), `pgTAP` with `is_empty(another_tenant)`.

---

## 5. Decision Summary

Use **stdlib `net/http` + `chi` + `pgxpool` + `go-redis` + `slog` + `os.Root`** with **Supabase PG17 RLS `STABLE DEFINER` + `(select ...)` + Supavisor 6543** and **Redis 8 `HGETEX`/`DELEX` + `io-threads 8` + `volatile-lru`**. Old stack (`gorilla/mux` + `logrus` + `GET+EXPIRE` + DB-per-tenant) is verbose, slower, and leaks. ✅ Implemented: chi + stdlib hybrid (chi for middleware, stdlib mux where helpful), pgxpool with Min=2/Max=20/Lifetime=1h, go-redis v9, slog JSON, RLS via `set_config('app.tenant_id', ..., true)` per transaction. ✅

---

## 6. Feature Platform ✅

Every capability a client can turn on is a **feature**. The platform is one registry + one gate + one builder.

```
internal/features/
  registry.go     Register(f Feature) + Names/Get/AllowedForVertical
  defaults.go     vertical -> default feature set (12 verticals)
  gate.go         RequireFeature(name) middleware, reads cached ClientModel
  builder.go      BuildVerticalRouter(deps, vertical) returns http.Handler
  init.go         features.Init() hook (cross-cutting init)
  clientctx/      owns the per-request context key (avoids import cycle)
  shared/
    records/      the §6.2 engine (always available)
    menu/         kind=menu_item, payload {name, price}
    booking/      kind=booking, payload {name, date|datetime}
    review/       kind=review, payload {rating 1..5, author}
    article/      kind=article, payload {title, body}
    promo/        kind=promo, payload {title, discount}
  clients/
    _template/    copy-paste starter for per-tenant custom features
```

### Tiers

- **Core** — `records`. Always mounted at `/{vertical}/records/...`. Never gated.
- **Shared** — `menu`, `booking`, etc. Mounted at `/{vertical}/{feature}/...` when the client's `features[]` includes the name. Validator runs on POST/PATCH.
- **Custom** — per-tenant. Lives in `clients/<tenant-slug>/`. Same URL shape; isolation test required.

### Promotion rule

A custom feature becomes a candidate for **shared** promotion after it has been live for ≥2 months and used by ≥3 tenants. Promotion is a code move from `clients/<slug>/` to `shared/<name>/`; the migration is no-op (same `kind` value, same table).

### Custom feature checklist (escape hatch)

- [ ] Migration with its own table (or shared `tenant_records` with a unique `kind` value) + RLS if it adds a table.
- [ ] `VerticalSlugs` set to the tenant's vertical (or `nil` for cross-vertical).
- [ ] `Name` lowercase, short, no collisions.
- [ ] `Kind` distinct from any other feature's kind.
- [ ] `ValidatePayload` enforces every required field.
- [ ] **Isolation test (`feature_test.go`) green** — tenant B must not see tenant A's data via this feature.
- [ ] `README.md` per feature, status (live/promoted/archived), owner tenant, why it exists.

### Gate performance

The gate reads the cached `*cache.ClientModel` from context (set by `withClientAuth` once per request). It performs two in-memory checks (`name ∈ features` and `AllowedForVertical(name, vertical)`) and rejects 403 if either fails. **Zero DB hit, zero Redis hit.**

---

## 7. Shared-DB Tenant Rule ✅

This proxy shares one Postgres with other tools (DFDWebsite Edge Functions, future BI jobs). The hard rules are:

1. **Every query must set the GUC** `app.tenant_id` before touching `tenant_records`. In the Go proxy: `withTenantTx(ctx, tenantID, fn)`. In Edge Functions: a `set_config('app.tenant_id', $1, true)` call at the start of every request handler.
2. **The `service_role` Postgres role is banned on tenant tables.** The service role bypasses RLS by definition; it must not be used to read or write `clients`, `tenant_records`, or `usage`. Build/Edge Function integrations that need admin access (DDL, superuser-only maintenance) use a separate superuser DSN, never `service_role`.
3. **The runtime pool connects as `app_runtime`** (Postgres role with `SELECT` on `clients` and `DML` on `tenant_records`+`usage`). Migrations and admin endpoints connect as a superuser via `DATABASE_ADMIN_URL`.
4. **RLS policies are PERMISSIVE on `tenant_records`**, not RESTRICTIVE — Postgres semantics: a RESTRICTIVE policy without a PERMISSIVE one denies everything; we use one PERMISSIVE policy that checks `current_setting('app.tenant_id') = tenant_id`.

---

## 8. Redis Degradation Matrix ✅

Redis is a hot layer, not the source of truth. Every key has a DB fallback path; the matrix documents the behavior when Redis is down or evicted.

| Subsystem | What Redis caches | Behavior when Redis is down | Behavior on cache miss |
|---|---|---|---|
| `cache.Client` (token → ClientModel) | `ck:{sha256}` for 15m | Cache lookup fails → straight to DB. Latency +5-10ms; throughput unaffected. | DB returns ClientModel; populate cache. |
| `quota` (monthly usage) | `quota:{client_id}:{YYYYMM}` for 30s | Falls back to `SUM(tokens_in+tokens_out) FROM usage` (slower but correct). | Same fallback path. |
| `middleware.RateLimit` | `rl:{client_id or ip}:{YYYYMMDDHHmm}` for 60s | **Fail open.** Allows the request. We do not block traffic on a Redis hiccup. | INCR creates the key. |
| `usage.Record` (post-call write) | `quota:{client_id}:{YYYYMM}` bump (eventual consistency) | Falls back to DB write only. The DB row is the source of truth; the cache is a hint. | DB-only path. |
| Feature toggle (`PATCH features`) | `ck:{hash}` eviction | Eviction is a `DEL`; if Redis is down, the cached entry lives up to 15m. New features may be blocked during that window. | Operator can wait 15m, or restart the proxy. **In production this is acceptable** — feature toggles are rare. |

The 15-minute worst case for feature toggles is why we keep the cache TTL short. If a tenant needs a feature turned on urgently, the operator can flush that one key:

```bash
redis-cli -h <host> DEL "ck:<sha256>"
```

---

## 9. Secrets Policy ✅

- All DSNs and the admin key come from the environment, never from a config file checked into the repo.
- `internal/config.(*Config).String()` redacts every secret for slog output (DSNs become `postgres://***@host`, admin key becomes `loca***`).
- The `slog` handler also redacts `Authorization` and any key matching `*password*`/`*secret*`/`*token*` on its way through.
- Sentry's `BeforeSend` callback runs the same redaction before transmitting.
- The raw client token is returned by `POST /v1/admin/clients` exactly once and never stored. The proxy stores `sha256(token)`.

## 10. Pool Configuration ✅

- `DB_POOL_MIN` (default 2), `DB_POOL_MAX` (default 20): sized per replica.
- `DB_USE_SIMPLE_PROTOCOL=true`: required when connecting through Supabase Supavisor 6543 (transaction-mode pooler) which does not support extended-query prepared statements.
- Admin DSN (`DATABASE_ADMIN_URL`) points at the direct connection (Supabase 5432) and is used only for migrations + admin endpoints.
- Pool config is applied in `internal/db.NewPoolWithOptions` and wired from `cmd/server/main.go`. The runtime pool is **always** the non-superuser `app_runtime` role.

For local Laragon dev, the default `DB_USE_SIMPLE_PROTOCOL=false` is fine; the connection is direct.

## 11. Custom-Table Features in Practice (Phase 4 — sports vertical) ✅

Phase 4 proved the custom-table feature path end-to-end on the `sports` vertical. The pattern, extracted from what actually shipped:

### 11.1 The three feature kinds (by storage)

| Kind | Storage | Example |
|---|---|---|
| Records-adapter | `tenant_records` with a fixed `kind` | `menu`, `booking` (shared) |
| Route-only | custom table(s), own handlers | `sports/facilities`, `sports/bookings` |
| Hybrid | custom table + records for lists | (future) |

A feature with `Kind: ""` + `Routes` gets its own subrouter under `/v1/{vertical}/{name}` (builder.go), gated by `RequireFeature` like everything else. The feature receives `features.BuilderDeps{Pool, Cache}` at build time.

### 11.2 Every custom table follows the same shape

```sql
CREATE TABLE <name> (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id  text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  ...
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE <name> ENABLE ROW LEVEL SECURITY;
ALTER TABLE <name> FORCE ROW LEVEL SECURITY;
CREATE POLICY <name>_isolation ON <name> FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
GRANT SELECT, INSERT, UPDATE, DELETE ON <name> TO app_runtime;
```

Handlers run inside `records.WithTenantTx` which sets the GUC per transaction — defense in depth under the RLS policy.

### 11.3 The double-booking exclusion constraint (the hard part)

```sql
CREATE EXTENSION IF NOT EXISTS btree_gist;  -- admin pool, superuser
CONSTRAINT sports_bookings_no_overlap EXCLUDE USING gist (
  tenant_id WITH =,
  facility_id WITH =,
  tsrange(date + start_time, date + end_time) WITH &&
)
```

- **Why `tsrange` not `tstzrange`:** the implicit `timestamp → timestamptz` cast in `tstzrange` is timezone-dependent (not `IMMUTABLE`), and Postgres forbids non-immutable expressions in index expressions. Venue slots are local-time — timezone has no business meaning.
- **Half-open `[start, end)`:** back-to-back slots are allowed; overlaps are rejected by the DB. The handler maps `23P01` (exclusion_violation) → `409 {"error":"slot already booked"}`.
- The concurrency test (`TestBookings_DoubleBooking`) fires two real HTTP requests at the same slot and asserts exactly one 201 + one 409 + one DB row.

### 11.4 The notify queue (booking never depends on WhatsApp)

`internal/notify/` = in-process channel + worker goroutine + retry/backoff (`Sender` interface). Confirmed booking → `Enqueue` (non-blocking, never fails the request) → worker sends with 3 attempts, exponential backoff. `WaMeSender` produces a `wa.me` deep link (zero credentials); the interface isolates a future Meta/Twilio swap to one file. If the server ever scales to multiple instances, swap the channel for Redis streams — `Sender` stays.

### 11.5 What Phase 4 proved

- The custom-table path (migration + RLS + grants + isolation test) works on four real tables.
- The `app` role test harness needed explicit grants on the new tables (`bootstrapAppRole` in every feature test).
- The `Features.Routes` signature had to grow deps (`Routes func(chi.Router, BuilderDeps)`) — the old `func(chi.Router)` couldn't hand a pool to custom handlers.
- **The test suite must run with `go test -p 1`.** The integration tests share one Postgres; with default parallelism, packages racing DELETE/INSERT on the same tables (different tenants, same table) hit Postgres's `tuple concurrently updated` (SQLSTATE XX000) at the page/index level. This is inherent to small shared tables, not a code bug — the Makefile's `test` target uses `-p 1` and the docs say so.

## 12. Telegram Agentic Layer (Phase 5) ✅

### 12.1 Why it's part of the same proxy

The agent is not a separate service. It is a **conversational wrapper over the feature platform**: the tool loop calls the same `WithTenantTx` + RLS + exclusion-constraint business logic the REST handlers use. That's what makes it cheap to build and safe to run — double-booking prevention, tenant isolation, quantity guards, and quota are inherited, not re-implemented.

```
Telegram message → agent_bindings (chat→client) → feature gate → agent tool loop → tools (REST logic) → reply
```

### 12.2 The three new pieces

| Piece | Files | What it does |
|---|---|---|
| Channel | `internal/telegram/` | Bot API client (getUpdates polling), chat→client resolver, message handler glue |
| Brain | `internal/agent/` | Tool loop (≤5 rounds), 11 tools, per-vertical system prompt, Redis history |
| Scheduler | `internal/reminder/` | Polls `agent_reminders` for due rows, sends via Telegram, marks sent |

### 12.3 The tool-calling convention

The LLM returns one JSON object per turn: `{"tool": "...", "args": {...}}` to call a tool, or `{"answer": "..."}` to reply. The loop feeds tool results back to the LLM, which decides whether to call again or answer. Portable across OpenAI-compatible providers (works with the mock in dev).

### 12.4 The agent_bindings RLS exception

`agent_bindings` is the ONE table whose RLS policy is `FOR SELECT USING (true)` — bindings are the cross-tenant lookup key (chat → client), not tenant data. The tenant boundary is enforced downstream: the feature gate on `client.features` and every tool's `WithTenantTx`. Admin (superuser) manages bindings.

### 12.5 The reminder scheduler's RLS story

The scheduler polls `agent_reminders` across ALL tenants, so it runs on the admin pool (internal infra, not a client endpoint). The per-tenant UPDATE (mark sent) still goes through `WithTenantTx` and is scoped by `tenant_id`.

### 12.6 The business model

- **Normal clients**: REST only. No `telegram` feature → the bot replies "not enabled".
- **Agentic clients**: admin adds `telegram` to `features[]` + binds the owner's chat → agent live.
- **Quota**: every agent LLM call records usage against the same monthly quota as REST — one billing line.

### 12.7 Security notes

- RLS is the hard boundary: a prompt-injected message can only reach the tenant's own data.
- Staff payments are a **ledger** (`agent_staff_payments`) — no money movement. Real payouts = a future phase with a payment provider.
- History lives in Redis (`ag:{tenant_id}`, 24h TTL). Fine for one instance; swap to Postgres if multi-instance.


