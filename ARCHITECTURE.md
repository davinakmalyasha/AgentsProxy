# ARCHITECTURE — AgentsProxy

> Why each version was chosen, and the design rules the code must keep satisfying.
> This is the decision record other agents (and you in six months) read to understand *why*.
>
> Companion docs: [`README.md`](README.md) = *what it is* ·
> [`docs/refinementNotes.md`](docs/refinementNotes.md) = *what to fix next*.
>
> `PRD.md`, `STANDARDS.md`, `DONE.md` and `docs/phase3-feature-platform.md` were folded into
> this file on 2026-10-03. Section numbering §1–§13 was preserved deliberately, because code
> comments reference it (`031_delivery_roles.sql` cites §7 rule 2).

---

## 1. Stack Snapshot (pinned at build time)

| Layer | Choice | Version | Why not the alternative |
|---|---|---|---|
| **Language** | Go | `1.27.0` | Node already powers the DFD site, but the gateway wants a binary and GC control. Python is great for agents, not for a 15 MB proxy. |
| **Router** | `net/http` + `chi/v5` thin wrapper | `v5.3.2` | Pure stdlib is verbose for nested route groups with scoped middleware. Gin/Fiber impose a custom context and reflection overhead. `chi` is an `http.Handler`, so stdlib handlers drop in. **See §2.1 — chi is the router here.** |
| **DB driver** | `jackc/pgx/v5` + `pgxpool` | `v5.10.0` | `database/sql` + `lib/pq` is the text protocol and slower. GORM hides SQL, making injection harder to audit. pgx is binary protocol with prepared statements by default. |
| **Cache** | `redis/go-redis/v9` | `v9.22.0` | `rueidis` is faster (auto-pipelining) but `go-redis` is the official client with stable docs. Either is defensible — pin one. |
| **DB (prod)** | Supabase Postgres | `17` | PG15 EOL mid-2026. PG17: faster `VACUUM`, `MERGE`, `pg_stat_io`, 700+ fixes. |
| **DB (local/CI)** | Laragon / `postgres:16-alpine` | `16` | What the dev box and CI services run. Prod-only features must not be assumed in migrations. |
| **Cache server** | Redis | `8.x` (prod `8.10.1`) | 7 would work; 8 brings 1-RTT atomics, io-threads, and compact hashes — free perf. |
| **Logging** | `log/slog` JSON | stdlib | Unstructured `log.Printf` is unparsable in Loki/ELK. No `logrus`, no `zap`. |
| **File access** | `os.Root` | stdlib (Go 1.24+) | Blocks symlink escape; never `filepath.Join + Clean + prefix`. |
| **Auth** | `bcrypt` (`x/crypto`) + DB sessions | — | No Supabase Auth; tenants are the isolation unit, not end users. |
| **Secrets at rest** | AES-256-GCM, versioned keyring | — | Bot tokens must be recoverable by two runtimes (Go + Deno), so the wire format is fixed at `0x01 ‖ nonce(12) ‖ ct‖tag`. |

---

## 2. Go 1.27 — what actually matters here

### 2.1 Routing: chi first, stdlib where it helps

Go 1.22+ gave `net/http` a real enhanced `ServeMux`:

```go
mux.HandleFunc("GET /v1/agents/{id}", getAgent)   // method + path param in the pattern
mux.HandleFunc("POST /v1/agents", createAgent)
mux.HandleFunc("/files/{path...}", serveFile)     // ... wildcard
mux.HandleFunc("/posts/{$}", listPosts)           // {$} = exact trailing slash
id := r.PathValue("id")
```

Unmatched methods return `405` with a correct `Allow` header, and both pattern and path are
unescaped segment-by-segment (`/%2F` is two segments, fixing old #21955).

**Decision: keep `chi`.** This service is not flat — it has `/v1` under a rate limiter, `/v1/admin`
under admin auth *and* a different DB pool, `/v1/agent/*` under a separate rate-limit scope, and
thirteen dynamically-built verticals whose routes are assembled at runtime from a feature
registry. That is precisely the nested-group-with-scoped-middleware case stdlib ServeMux handles
worst. `chi` is `http.Handler`-compatible, so a stdlib mux can still be mounted inside it where
a group is genuinely flat. (An earlier revision of this file claimed ServeMux "replaces chi" —
that was wrong and contradicted the code.)

Go 1.25 added `http.NewCrossOriginProtection()` — stdlib CSRF defense via `Sec-Fetch-Site` with
an `Origin` fallback. Not enabled: this is an API-only surface for static client sites, and CORS
middleware is the correct control. Revisit if a browser-facing admin UI is added.

### 2.2 `log/slog`

```go
h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
    Level: slog.LevelInfo, AddSource: true,
    ReplaceAttr: func(g []string, a slog.Attr) slog.Attr {
        if a.Key == "Authorization" { return slog.String("Authorization", "[REDACTED]") }
        return a
    },
})
```

JSON is indexed and filterable by `level` / `request_id` / `trace_id`, which is what makes the
`RUNBOOK.md` log queries possible at all.

### 2.3 Security

- **`crypto/tls` post-quantum default** — the hybrid ML-KEM-768 key exchange is enabled when
  `CurvePreferences` is nil. **Never set `CipherSuites` or `CurvePreferences` manually**; that
  preserves future defaults. Server hardening lives in `cmd/server/main.go`:
  `ReadHeaderTimeout 5s`, `ReadTimeout 10s`, `WriteTimeout 90s`, `IdleTimeout 120s`.
- **`crypto/mldsa`** (Go 1.27) — FIPS 204 signatures wired into `crypto/x509` and `crypto/tls`.
  Not needed today; relevant if you ever sign artifacts with a Go binary and want to drop a
  dependency.
- **`os.Root`** — confined filesystem access for the generated-report path. See §9.

### 2.4 Performance (free, already on)

- **Swiss Tables `map`** (1.24+) — default. Benefits `map[string]*ClientModel` style caches.
- **Green Tea GC** (1.26+) — default; materially lower GC overhead. Tune with `GOMEMLIMIT` under
  memory pressure rather than by disabling.
- **Size-specialized allocation** (1.27) — allocations under 80 bytes are up to ~30% cheaper.
  ~1% whole-program. No action.
- **Container-aware `GOMAXPROCS`** (1.25+) — the runtime reads the cgroup CPU limit, replacing
  the `automaxprocs` sidecar on Railway/K8s.
- **PGO** — build with `-pgo=auto` once a CPU profile exists. Single-digit percent, zero code.

### 2.5 Use selectively / avoid

- `iter`, `unique`, `slices`/`maps` — idiomatic, not urgent.
- `encoding/json/v2` — **v1 is already backed by the v2 implementation**, so unmarshalling is
  faster today without any change. Import `v2` explicitly only if you want its stricter defaults
  (rejects duplicate object keys and invalid UTF-8 — relevant to client-supplied JSON).
- **Do NOT build on `GOEXPERIMENT=simd`.** Experimental; the API will change.

---

## 3. Redis 8 — what actually matters here

### 3.1 I/O threading

Main thread executes; I/O threads handle read/parse/write. Worth enabling (`io-threads 8`,
`io-threads-do-reads yes`) because this proxy is ~90% `GET` (token cache, quota, rate limit) —
the direct beneficiary. Set `io-threads` to physical cores, max 8; beyond that adds contention.

### 3.2 Atomic helpers — 2 RTT → 1 RTT

| Old | New | Use here |
|---|---|---|
| `GET` + `EXPIRE` (2 RTT, racy) | `HGETEX key EX 60 FIELDS 1 field` (1 RTT, atomic) | Client-cache refresh: `HGETEX ck:{hash} EX 900 FIELDS 1 data` |
| `MSET` + `EXPIRE` (2 RTT) | `MSETEX numkeys k v [NX][EX]` | Batch-warm `ck:*` after a deploy |
| `GET` + `DEL` with a check | `DELEX key IFEQ value` + `DIGEST` | Idempotency-lock release without Lua |
| Key explosion per window | `HEXPIRE` per-field TTL on one hash | Per-second quota windows as hash fields |

> **Status: designed, not implemented.** `internal/cache/` still does the two-step `GET`+`EXPIRE`.
> Tracked as `refinementNotes` R9.

### 3.3 Rate limiting — Redis token bucket

**Never** `INCR` then non-atomic `EXPIRE`: that allows a `limit×2` burst across a minute
boundary. Use a single Lua script (`EVALSHA`), which is sub-millisecond and far beyond this
service's needs:

```lua
-- token-bucket.lua — args: capacity, refillRate, nowSec
local data = redis.call('HMGET', KEYS[1], 'tokens', 'last_refill')
local tokens = tonumber(data[1]) or tonumber(ARGV[1])   -- capacity on first call
local last   = tonumber(data[2]) or tonumber(ARGV[3])
local elapsed = tonumber(ARGV[3]) - last
tokens = math.min(tonumber(ARGV[1]), tokens + elapsed * tonumber(ARGV[2]))
local allowed = tokens >= 1
if allowed then tokens = tokens - 1 end
redis.call('HMSET', KEYS[1], 'tokens', tokens, 'last_refill', ARGV[3])
redis.call('PEXPIRE', KEYS[1], math.ceil(tonumber(ARGV[1]) / tonumber(ARGV[2]) * 1000) + 1000)
return {allowed and 1 or 0, tokens, math.ceil((1 - tokens) / tonumber(ARGV[2]))}
```

Hybrid with a sliding-window counter (two weighted string keys) if a hard RPM ceiling is
required on top of the burst allowance.

> **Status: designed, not implemented.** `internal/middleware/ratelimit.go` still uses the
> racy pair. Tracked as R9.

### 3.4 Other 8.x wins

- `volatile-lrm` / `allkeys-lrm` eviction — least-recently-*modified* suits quota keys better
  than LRU.
- Hot-key detection and `CLUSTER SLOT-STATS` — catch a single tenant hammering `quota:{id}`.
- Reduced `HASH` memory and single-allocation `kvobj` — more `ck:*` keys per unit of RAM.

### 3.5 TLS and ACL

TLS works alongside `io-threads` since 8.0 at a real throughput cost — co-locate the cache in
the same AZ. Three least-privilege users is the right shape for this proxy:

```conf
ACL SETUSER proxy_cache      on >$STRONG ~cache:* ~ck:* +@read +mget +get +exists +ttl resetchannels &*
ACL SETUSER proxy_quota      on >$STRONG ~quota:* ~rl:* ~ck:* +@write +@read +eval +evalsha +incr +expire +pexpire +hexpire +hgetex +hsetex resetchannels &*
ACL SETUSER proxy_invalidator on >$STRONG ~cache:* ~ck:* +del +unlink +scan resetchannels &*
```

Audit candidate ACLs with `ACL DRYRUN`.

---

## 4. Supabase Postgres — one DB, many tenants

### 4.1 Why shared schema + RLS beats DB-per-tenant

| Model | Cost/tenant | Migrations | Pooling | Limit |
|---|---|---|---|---|
| DB-per-tenant | $25+/mo | ×N | ×N `max_connections` (2–3 MB per idle backend) | 100 tenants = OOM |
| Schema-per-tenant | $5–15/mo | ×N | Complex | Hard ceiling |
| **Shared schema + `tenant_id` + RLS** | pennies | ×1 | **one pool multiplexes 10k → 20 backends** | **Correct for this workload** |

The deciding factor is not cost, it is that RLS makes the tenant boundary **unforgeable from the
application layer**. A handler bug that forgets a `WHERE` clause cannot leak another tenant,
because the policy rejects the row.

### 4.2 RLS rules (miss one = leak)

1. **Enable on every public table.** A policy without `ENABLE ROW LEVEL SECURITY` is a no-op.
   CI check: no `public` table may have `rowsecurity = false`. The check is scoped to `public`
   on purpose — `pg_catalog` and extension schemas are not ours, while our own tables
   (`notify_jobs` included) are `FORCE`d like the rest.
2. **Fix the grants.** Legacy `anon`/`authenticated` auto-grants must be revoked down to the
   operations actually needed. A policy is not a grant.
3. **One policy per operation** (`FOR SELECT/INSERT/UPDATE/DELETE`), never `FOR ALL` in prod.
   `USING` = rows visible; `WITH CHECK` = rows writable. `UPDATE` needs both.
4. **`AS RESTRICTIVE` for the tenant boundary.** Restrictive policies combine with `AND`,
   permissive with `OR` — so a later permissive policy cannot leak by union. *(See §7 rule 4
   for why `tenant_records` is the documented exception.)*
5. **DRY via `SECURITY DEFINER STABLE`:**
   ```sql
   CREATE OR REPLACE FUNCTION private.get_user_tenant_ids() RETURNS SETOF uuid
   LANGUAGE sql STABLE SECURITY DEFINER SET search_path='' AS $$
     SELECT tenant_id FROM public.memberships WHERE user_id = (select auth.uid());
   $$;
   ```
   `search_path=''` plus schema-qualified names prevents hijack; the definer owning the
   function avoids RLS recursion.
6. **Performance: wrap in a subquery.** Bare `auth.uid()` is evaluated per row; `(select
   auth.uid())` becomes an `InitPlan` cached once per statement. This is one of the largest
   single wins available in RLS.
7. **Denormalize `tenant_id` onto child tables** — otherwise every policy needs a JOIN.
8. **Index the policy columns.** Without them naive RLS is dramatically slower. A `(tenant_id,
   kind)` index plus a `STABLE` definer brings the overhead down to acceptable.

### 4.3 Pooling — `pgxpool` and Supavisor compose

- **`pgxpool`** bounds connections per replica and amortizes TLS/auth. Defaults: `MinConns 2`,
  `MaxConns 20`, `MaxConnLifetime 1h`, health check 1m. See §10.
- **Supavisor** (Supabase shared pooler):
  - **Transaction mode, port 6543** → runtime on serverless. `BEGIN`/`COMMIT` only. No
    `LISTEN`, no session `pg_advisory_lock` (use `pg_advisory_xact_lock`), no bare `SET` (use
    `SET LOCAL`), no cross-transaction `TEMP TABLE`, and `prepare:false`.
  - **Session mode, port 5432** → migrations, `pg_dump`, `LISTEN`.

### 4.4 Platform features in use

- **Edge Functions (Deno)** — host the reminder and notification **delivery** drivers on
  serverless. See §13.
- **`pg_cron` + `pg_net`** — the clock for time-driven work on serverless. Cron bodies are
  **gated on an `exists()` probe**, so an idle system spends one indexed query per tick and
  *zero* function invocations. That matters against Supabase's free-tier invocation allowance.
- **`pgmq` was evaluated and rejected.** Migrations run in a single transaction
  (`internal/db/migrate.go`), so depending on a Supabase-only extension would abort the entire
  chain on Laragon or any VPS. `notify_jobs` reimplements the same model — read counter plus
  visibility timeout — in four statements and runs on any Postgres. **Do not reintroduce an
  extension dependency into a migration.**
  The nuance worth keeping: pgmq ships **no DLQ, no retry limit, and no failure alerting**, so
  adopting it would have meant building the parts this repo already has in migrations 029–031.
  What *is* worth borrowing is the discipline of making the database check on itself — a
  scheduled assertion on queue depth and oldest-message age. That is the failure mode this
  architecture is actually exposed to.

### 4.5 Schema map

44 migrations (`001`–`044`; 45 files on disk including `013b`), applied by an
embedded-SQL runner behind `pg_advisory_xact_lock(734561)` so
concurrent boots cannot race.

| Domain | Migrations | Contents |
|---|---|---|
| Tenancy core | `001`–`003` | `schema_migrations`, `clients` (token hash, quota, status), `usage`, `tenant_records` |
| Feature platform + RBAC | `004`–`006` | `clients.vertical`, `clients.features[]`, `app_runtime` role split, booking date index |
| Sports venue | `007`–`010`, `018`, `019` | facilities, bookings, inventory, members, court blocks, recurring series |
| Sports money | `014`–`017`, `020` | payments, pricing rules, expenses, customers, kantin products/sales/stock movements |
| Sports identity | `021`, `022` | `sports_users`, `sports_sessions` |
| Gap-filling | `013b`, `023`, `028`, `034`, `035`, `037`–`039`, `044` | booking lifecycle, WIB day-expression indexes, venue hours, price recalc trigger, court-block guard, series↔booking link, kantin soft-void, kantin tenant indexes |
| Agent (Telegram removed by `042`) | `011`–`013`, `024`–`026`, `042` | `agent_bindings`, `agent_reminders`, `agent_staff_payments` (`telegram_bots` created by `025`–`026`, dropped by `042`) |
| Delivery | `029`–`031`, `036` | `notify_jobs`, reminder claim protocol, `delivery_worker` / `delivery_caller` least-privilege roles |
| Sessions / RLS / misc | `027` (retired stub), `032`, `033`, `040`, `041`, `043` | session tenant scoping, `clients`/`usage` policies, `sports_users.phone`, ADK sessions (`041`), report objects (`043`) |

> **Two known sharp edges.** `027` is a deliberately retired no-op stub, kept in place so the
> sequence stays gap-free. And because migrations run in one transaction, **a single bad
> migration aborts the chain including everything after it** — which on Vercel means re-running
> the check on every cold start. The lock makes it safe; it does not make it cheap.

---

## 5. Decision Summary

Use **`chi` + stdlib `net/http`**, **`pgxpool`**, **`go-redis`**, **`slog`**, **`os.Root`**,
with **Supabase PG RLS (`STABLE DEFINER` + `(select ...)`) + Supavisor 6543** for the runtime
and **Redis 8** for the hot layer. Tenant isolation is enforced by the database, so the app
layer is free to make mistakes. The old shape (`gorilla/mux` + `logrus` + `GET`+`EXPIRE` +
DB-per-tenant) was verbose, slower, and leaky.

---

## 6. Feature Platform

Every capability a client can turn on is a **feature**. The platform is one registry, one gate,
one builder.

```
internal/features/
  registry.go     Register(f Feature) + Names/Get/AllowedForVertical
                  panics on a duplicate feature name or duplicate route pattern — fail at boot, not at request
  defaults.go     vertical -> default feature set
  gate.go         RequireFeature(name) middleware; reads the cached ClientModel from context
  builder.go      BuildVerticalRouter(deps, vertical) -> http.Handler
  init.go         features.Init() hook
  clientctx/      owns the per-request context key (exists to break an import cycle)
  shared/
    records/      the generic CRUD engine — always mounted, never gated
    menu/ booking/ review/ article/ promo/     records-backed, each with a fixed `kind` + payload validator
  _template/      copy-paste starter for a per-tenant custom feature
tenants/<slug>/backend/
  <name>/         14 custom-table features in tenants/centrum-badminton/backend/ (incl. testutil),
                  all VerticalSlugs = ["sports"]
```

### 6.1 Tiers

- **Core** — `records`. Always mounted at `/{vertical}/records/...`. Never gated.
- **Shared** — `menu`, `booking`, `review`, `article`, `promo`. Mounted at
  `/{vertical}/{feature}/...` when the client's `features[]` includes the name; the validator runs
  on POST/PATCH.
- **Custom** — per tenant, under `tenants/<slug>/backend/<name>/` (e.g. `tenants/centrum-badminton/backend/`). Same URL shape; isolation test required.

### 6.2 Gate behaviour

`RequireFeature(name)` reads the cached `*cache.ClientModel` that `withClientAuth` already put in
context and performs two in-memory checks: `name ∈ client.features` and
`AllowedForVertical(name, vertical)`. **Zero DB hit, zero Redis hit.** A `403` means either check
failed.

### 6.3 Admin surface

`POST /v1/admin/clients` accepts optional `vertical` (default `fnb`) and `features` (default =
`DefaultSetFor(vertical)`); an unknown feature name is a `400`. `PATCH` accepts `features` and
evicts the cached model.

### 6.4 Promotion rule

A custom feature becomes a candidate for shared promotion after ≥2 months live and ≥3 tenants
using it. Promotion is a **code move** from `tenants/<slug>/backend/<name>/` to `internal/features/shared/<name>/` — the migration
is a no-op, because the `kind` value and table are unchanged.

### 6.5 Middleware order matters

Identity must resolve **before** the per-client rate limiter, because the limiter buckets on
`CtxClientID`. `BuildVerticalRouter` mounts `WithIdentity` then `RateLimitClient(redis, n,
"client:"+vertical)`. Reordering silently degrades the limit to per-IP.

---

## 7. Shared-DB Tenant Rule

This Postgres is **shared with other tools** (DFDWebsite Edge Functions, future BI jobs). The
hard rules:

1. **Every query must set the GUC `app.tenant_id`** before touching `tenant_records`. In the Go
   proxy that is `withTenantTx(ctx, tenantID, fn)`; in Edge Functions it is
   `set_config('app.tenant_id', $1, true)` at the start of every request handler.
2. **The `service_role` Postgres role is banned on tenant tables.** It bypasses RLS by
   definition; it must never read or write `clients`, `tenant_records`, or `usage`. Integrations
   needing admin access (DDL, superuser-only maintenance) use the separate superuser DSN,
   **never `service_role`**.
3. **The runtime pool connects as `app_runtime`** — a Postgres role with `SELECT` on `clients`
   and DML on `tenant_records` + `usage`. Migrations and admin endpoints use a superuser via
   `DATABASE_ADMIN_URL`. Note the admin routes run on a *different pool* for exactly this reason.
4. **RLS policies on `tenant_records` are PERMISSIVE, not RESTRICTIVE.** Postgres semantics: a
   RESTRICTIVE policy with no PERMISSIVE partner denies everything. The design uses one
   PERMISSIVE policy checking `current_setting('app.tenant_id') = tenant_id`. This is a
   deliberate, documented exception to §4.2 rule 4 — re-verify it on every policy edit.

---

## 8. Redis Degradation Matrix

Redis is a hot layer, **not** the source of truth. Every key has a Postgres fallback.

### 8.1 Keyspace

| Key | Value | TTL | Notes |
|---|---|---|---|
| `ck:{sha256(token)}` | `{id, tenant_id, status, quota, name}` | 900s | Evicted (`DEL`) on admin PATCH |
| `rl:{clientID}:{YYYYMMDDHHmm}` | counter | 60s | One key per minute per client; old keys self-expire, so there is no cleanup job |
| `quota:{clientID}:{YYYYMM}` | `SUM(tokens_in+tokens_out)` for the month | 30s | Best-effort `INCRBY` on each usage insert, TTL as the safety net |

**Never store a raw token in Redis** — only `sha256Hex(token)` as the key.

### 8.2 Behavior when Redis is down

| Subsystem | Cached | Redis down | On miss |
|---|---|---|---|
| `cache.Client` | `ck:{hash}`, 15m | falls straight through to the DB; +5–10 ms latency | DB read, then repopulate |
| `quota` | `quota:{id}:{YYYYMM}`, 30s | falls back to `SUM(...) FROM usage` — slower, still correct | same fallback |
| `middleware.RateLimit` | `rl:{id}:{minute}`, 60s | **fail open** — a Redis hiccup must not block traffic | `INCR` creates the key |
| `usage.Record` | quota bump | DB write only; the row is truth, the cache is a hint | DB-only path |
| Feature toggle | `ck:{hash}` eviction | stale entry lives up to 15m; new features may be briefly blocked | operator flushes the one key, or restarts |

The 15-minute worst case for feature toggles is exactly why the TTL is short. Urgent case:

```bash
redis-cli -h <host> DEL "ck:<sha256>"
```

---

## 9. Secrets Policy

- Every DSN and the admin key come from the environment. No config file in the repo.
- `internal/config.(*Config).String()` redacts secrets for log output (DSNs become
  `postgres://***@host`).
- The `slog` handler additionally redacts `Authorization` and any key matching
  `*password*` / `*secret*` / `*token*`.
- Sentry's `BeforeSend` runs the same redaction **before transmitting**.
- The raw client token is returned by `POST /v1/admin/clients` **exactly once** and never
  stored — the proxy persists only `sha256(token)`.
- File access uses `os.Root`, not `filepath.Join + Clean + prefix`, to prevent symlink escape.
  The generated xlsx report path (`internal/api` report handler → `data/reports/<tenant>/`) is
  the place this must hold; it is token-addressed rather than tenant-addressed, which makes it
  worth re-auditing.
- Telegram bot vault removed by migration `042` (former §13.3 no longer applies).

---

## 10. Pool Configuration

- `DB_POOL_MIN` (default 2), `DB_POOL_MAX` (default 20) — **per replica**.
- `DB_USE_SIMPLE_PROTOCOL=true` is **required** through Supavisor 6543 (transaction-mode
  pooler), which does not support extended-query prepared statements.
- `DATABASE_ADMIN_URL` points at the direct connection (5432) and is used only for migrations
  and admin endpoints.
- Config lives in `internal/db.NewPoolWithOptions`, wired from `cmd/server/main.go`. The runtime
  pool is **always** the non-superuser `app_runtime` role.
- For local Laragon dev the defaults are fine — the connection is direct.

> **Serverless warning.** `DB_POOL_MAX` is per instance. On Vercel the default 20 × ~20
> concurrent lambdas would try to open 400 connections into Supabase. Set `DB_POOL_MAX=2` and
> `DB_POOL_MIN=0`. See `ops/RUNBOOK-vercel.md`.
>
> Note these three vars are read by `config.go` but **missing from `.env.example`** — a known
> documentation gap (`refinementNotes` C4/R5).

---

## 11. Custom-Table Features in Practice (the `sports` vertical)

The `sports` vertical is the pilot that proves the custom-table path and the hardest concurrency
problem on the platform: double-booking prevention.

### 11.1 Three feature shapes (by storage)

| Shape | Storage | Example |
|---|---|---|
| Records adapter | `tenant_records` with a fixed `kind` | `menu`, `booking` |
| Route-only | custom table(s) + own handlers | `sports/facilities`, `sports/bookings` |
| Hybrid | custom table + records for lists | (future) |

A feature with `Kind: ""` plus `Routes` receives a subrouter under `/v1/{vertical}/{name}`, gated
by `RequireFeature` like everything else, and gets `features.BuilderDeps{Pool, Cache}` at build
time. *(The `Routes` signature had to grow deps — the original `func(chi.Router)` could not hand
a pool to custom handlers.)*

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
ALTER TABLE <name> FORCE  ROW LEVEL SECURITY;
CREATE POLICY <name>_isolation ON <name> FOR ALL
  USING      (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
GRANT SELECT, INSERT, UPDATE, DELETE ON <name> TO app_runtime;
```

Handlers run inside `records.WithTenantTx`, which sets the GUC per transaction — defense in depth
*under* the policy, not instead of it.

### 11.3 The double-booking exclusion constraint (the hard part)

```sql
CREATE EXTENSION IF NOT EXISTS btree_gist;   -- admin pool, superuser
CONSTRAINT sports_bookings_no_overlap EXCLUDE USING gist (
  tenant_id  WITH =,
  facility_id WITH =,
  tsrange(date + start_time, date + end_time) WITH &&
)
```

- **Why `tsrange`, not `tstzrange`:** the implicit `timestamp → timestamptz` cast in `tstzrange`
  is timezone-dependent and therefore not `IMMUTABLE`, and Postgres forbids non-immutable
  expressions in index expressions. Venue slots are local time — timezone carries no meaning.
- **Half-open `[start, end)`** so back-to-back slots are legal; the handler maps `23P01`
  (`exclusion_violation`) to `409 {"error":"slot already booked"}`.
- Concurrency test `TestBookings_DoubleBooking` fires two real HTTP requests at the same slot and
  asserts exactly one `201`, one `409`, and one row.

### 11.4 The notify queue (booking never depends on WhatsApp)

A confirmed booking calls `Enqueue` — non-blocking, never fails the request — and a worker sends
with retries and exponential backoff behind the `Sender` interface. `WaMeSender` currently
produces a `wa.me` deep link with zero credentials, which is why booking confirmations do not yet
reach a real customer (`refinementNotes` C6/R15). If the deployment ever scales to multiple
instances, swap the in-process channel for Redis Streams; `Sender` stays.

### 11.5 What the pilot proved

- The custom-table path (migration + RLS + grants + isolation test) works across the 14 backend dirs in `tenants/centrum-badminton/backend/` (incl. `testutil`).
- The `app` role test harness needs explicit grants for every new table (`bootstrapAppRole` in
  each feature test). Easy to forget; failing loudly would be better.
- **The suite must run `go test -p 1`.** Integration tests share one Postgres; with default
  parallelism, packages racing DELETE/INSERT on the same tables hit `tuple concurrently updated`
  (SQLSTATE XX000) at the page/index level. Inherent to small shared tables, not a code bug.

---

## 12. Agentic Layer (Telegram removed)

> **Status (2026-10-03):** the Telegram channel is deleted — `internal/telegram/` is gone
> and migration `042_drop_telegram.sql` drops `telegram_bots` / `agent_reminders.bot_id`.
> The agent runner now lives in `internal/adkrunner/` with per-tenant tools under
> `tenants/<slug>/backend/` (e.g. `tenants/centrum-badminton/backend/`).

### 12.1 Why it lives in the same proxy

The agent is **not a separate service**. It is a conversational wrapper over the feature
platform: the tool loop calls the same `WithTenantTx` + RLS + exclusion-constraint logic the REST
handlers use. Double-booking prevention, tenant isolation, quantity guards, and quota are all
**inherited rather than re-implemented** — that is what makes it cheap to build and safe to run.

```
client message → agent_bindings (chat→client) → feature gate → tool loop → tools (REST logic) → reply
```

### 12.2 The pieces

| Piece | Location | Role |
|---|---|---|
| Brain | `internal/adkrunner/` | Tool loop, per-role tool filtering, per-vertical system prompt, Redis history |
| Tools | `AgentsTools/` + `tenants/centrum-badminton/backend/` | The 19 tenant tools, dispatching into the same RLS path |
| Config | `internal/agentconfig/` + `AgentsSkills/`, `AgentsMemory/` | Per-tenant persona, skills, knowledge (MCP declaration removed with `internal/mcp`) |
| Scheduler | `internal/reminder/` | Due-reminder polling and delivery (§13) |

### 12.3 The tool-calling convention

The LLM returns one JSON object per turn: `{"tool":"...","args":{...}}` to call a tool, or
`{"answer":"..."}` to reply. The loop feeds results back and the model decides again. Portable
across OpenAI-compatible providers, which is why the fake-LLM tests work.

> This is also the seam where the highest-leverage cost change lives: tool definitions and the
> system prompt are a large, **static, per-tenant prefix**. See §17.

### 12.4 The `agent_bindings` RLS exception

`agent_bindings` is the **one** table whose policy is `FOR SELECT USING (true)` — bindings are the
cross-tenant lookup key (chat → client), not tenant data. The tenant boundary is enforced
downstream by the feature gate and every tool's `WithTenantTx`. Admin manages bindings.

### 12.5 The scheduler's RLS story

The reminder scheduler polls across **all** tenants, so it runs on the admin pool (internal
infrastructure, not a client endpoint). Its per-tenant UPDATE still goes through `WithTenantTx`
and is scoped by `tenant_id`.

### 12.6 Security notes

- RLS is the hard boundary: a prompt-injected message can only reach that tenant's own data.
- Staff payments are a **ledger** (`agent_staff_payments`), not money movement. Real payouts need
  a payment provider and are out of scope today.
- History lives in Redis (`ag:{tenant_id}`, 24h TTL) — fine for one instance; move to Postgres
  before running more than one.
- Role ceilings are enforced per request from the session/binding DB row, never from request parameters.

---

## 13. The Scheduler Runs On Postgres, Not In The Process

### 13.1 Why the clock moved

Reminders and notification sends are **time-driven**: nothing happens until a deadline passes.
On a long-running host the Go process owns that with a `time.Ticker`. On serverless it cannot —
the function is destroyed between requests, so a reminder set for 08:00 silently never fires.

Time passing is not an event. Nothing emits "it is 08:00". So *something* must watch the clock,
and on serverless the only always-on thing is the database. Hence `pg_cron`.

### 13.2 Two drivers, one claim

| | Long-running host | Serverless |
|---|---|---|
| Trigger | Go ticker, 60s | `pg_cron` → reminder Edge Function (**PENDING-REPAIR**: `deliver-reminders` is deleted, `reminder-tick` currently has no target) |
| Notify drain | Go ticker, 5s | DB Webhook on INSERT (instant) + cron backstop |
| Stale-claim reaper | Go boot path | `pg_cron` → `reap_stale_claims` (pure SQL, no HTTP) |

They are made mutually exclusive by an **atomic claim**, not by a lock:

```sql
-- migrations 030 / 031
UPDATE agent_reminders SET status = 'sending', claimed_at = clock_timestamp()
 WHERE id IN (SELECT id FROM agent_reminders
               WHERE status = 'pending' AND remind_at <= now()
               ORDER BY remind_at LIMIT 20 FOR UPDATE SKIP LOCKED)
```

Drivers only ever read `status = 'pending'`, so a claimed row is invisible to the other driver.
`claimed_at` doubles as an optimistic-concurrency token: completion writes are scoped
`AND status = 'sending' AND claimed_at = $2`, so a send that outlived the reaper window cannot
mark a row the reaper already handed back. `private.reap_stale_claims()` returns claims older than
10 minutes to `pending`; that window is many times the per-row send budget, so a slow-but-alive
driver is never robbed mid-send.

**Why not hold `FOR UPDATE` across the send?** The send is an HTTP call to the delivery target. Holding a
row lock for its duration means a long-running transaction on the shared pool. The status flip
gets the same mutual exclusion with no open transaction.

Asserted directly: `TestScheduler_TwoDriversDeliverEachRowOnce` fires two concurrent ticks
against a shared sender and requires every row to arrive exactly once.

`notify_jobs` needs no claim flag — a read counter plus a visibility timeout (the pgmq model)
gives it, and a driver that dies before delete makes the row reappear rather than lose it.

### 13.3 Least-privilege claim roles (Telegram vault removed)

`agent_reminders` is `FORCE ROW LEVEL SECURITY` with policy
`tenant_id = current_setting('app.tenant_id', true)`. FORCE applies to the table owner, so even a
`SECURITY DEFINER` function owned by `postgres` cannot read across tenants — a cross-tenant
sweeper has no single tenant GUC to set. The Go scheduler solves this with the admin pool.

The Supabase driver gets a **strictly narrower** path:

| Role | Attributes | Reach |
|---|---|---|
| `delivery_worker` | `NOLOGIN BYPASSRLS` | owns the claim functions, so they execute with its rights |
| `delivery_caller` | `LOGIN NOBYPASSRLS` | `EXECUTE` on the claim functions **and nothing else** — no `USAGE` on `public`, no table grants |

The ownership reassignment in migration 031 is load-bearing: without it the functions stay owned
by the migration runner and `SECURITY DEFINER` executes as superuser, bypassing every policy with
far more authority than intended. `BYPASSRLS` skips row *policies* but not *GRANTs*, so
`delivery_worker` also holds the narrow table grants it needs.

> **Telegram vault removed (2026-10-03):** migration `042` drops `telegram_bots` and the
> `token_enc` ciphertext path. The former AES-GCM vault paragraph no longer applies; the claim
> functions return reminder payloads without bot tokens (see `042_drop_telegram.sql`).

`pgcrypto` could not have done this: `pgp_sym_decrypt` expects OpenPGP message format and
`decrypt_iv` is raw AES-**CBC**. There is no raw AES-GCM in pgcrypto, which is exactly what rules
out a pure-SQL driver.

### 13.4 The invariant to protect

> **A row is delivered exactly once per attempt, by exactly one driver.**

The claim is the only thing holding that up, so **a second delivery path that does not claim will
double-send.** If you add one, route it through `private.claim_*` rather than selecting rows
directly. This is also the reason the Go and Deno implementations must stay in lockstep — see
`refinementNotes` C5.

---

## 14. API Design Rules

### 14.1 Auth surfaces (do not conflate)

| Header | Identity | Scope |
|---|---|---|
| `X-Client-Token` | a tenant/client | Full access to that tenant's data and features |
| `X-Session-Token` | a staff user (`owner` / `pegawai`) | Role-limited; `owner` writes money and schedule, `pegawai` is read-mostly |
| `Authorization: Bearer $ADMIN_API_KEY` | the operator | Admin + cross-tenant MCP tools. **Server-side only, never a client site.** |

### 14.2 `/v1/chat` handler order (no shortcuts)

1. Parse and validate: non-empty `messages`, `role ∈ {system,user,assistant}`, non-empty
   content, `client_token` present. Bad JSON → `400 {"error":"..."}`.
2. Auth: `HashToken` → cache lookup, falling back to the DB. Unknown → `401`. Not `active` → `403`.
3. Rate limit (global middleware already ran).
4. Quota → `429 {"error":"quota exceeded","resetAt":"<first of next month ISO>"}` + `Retry-After`.
5. Call the provider with a **60s** `context.WithTimeout`. Capture usage from the provider if
   present, else estimate. **Do not log the prompt or the token.**
6. Record usage: synchronous `INSERT` (source of truth) plus a best-effort Redis bump.
7. Return `{"reply":..., "usage":{...}}`. **Sanitize provider errors** — never echo raw; return
   `{"error":"provider error"}` with a `502`.

Required: `subtle.ConstantTimeCompare` on the hash, the 60s timeout, sanitized errors.

### 14.3 Records: cursor pagination

```
GET /v1/{vertical}/records?kind=&q=&before=&limit=&status=
```

`limit` defaults to 20, caps at 100. `before` is the opaque cursor from the previous page.
Soft-deleted rows are hidden unless `?status=deleted`. The response is
`{"items":[...],"next_cursor":"<base64>","has_more":bool}`.

Before **every** DML: set the GUC so RLS applies even if the handler forgets its `WHERE`.

### 14.4 Error shape

`400 {"error":"..."}` · `401`/`403` for auth · `404` for not-owned (never `403`, which would
confirm existence) · `409` for a domain conflict such as a double booking · `429` with
`Retry-After` · `500 {"error":"internal"}` with provider detail never included.

### 14.5 Streaming

`POST /v1/chat` with `Accept: text/event-stream` should proxy the provider's SSE frames through,
flushing per chunk. **Not implemented** — the longest-standing gap
(`refinementNotes` R7).

---

## 15. Security Standards (miss = fail)

| Rule | How | Why |
|---|---|---|
| Zero secrets in repo | `.env.example` documents every var; `.gitignore` covers `.env`, `bin/`, `*.log` | Clones and CI runners leak |
| Token storage | `sha256Hex` only; raw shown once, never stored, never logged | A DB leak stays useless |
| Constant-time compare | `subtle.ConstantTimeCompare` | Timing attack on a hash |
| SQL | `pgx` parameterized `$1,$2` only — never `fmt.Sprintf` into SQL | Injection |
| RLS | `ENABLE` + `FORCE` + GUC set before every DML | An app-layer bug cannot leak tenants |
| Grants | Revoke `anon`/`authenticated` auto-grants; policy **and** grant | A policy is not a grant |
| File access | `os.Root` | Symlink escape |
| Server hardening | `ReadHeaderTimeout 5s`, `ReadTimeout 10s`, `IdleTimeout 120s`; leave `CurvePreferences` nil | Slowloris; preserves PQC defaults |
| Headers | security-headers middleware, `CSP`, CORS from `ALLOW_ORIGINS` csv, credentials off | XSS, CORS |
| Rate limit | per token when authenticated, per IP when not | Brute-force guard |
| Validation | `vertical ∈ AllowedVerticals` (400 unknown), `kind` non-empty, `limit` ranges, JSON errors → `400` | Input fuzzing |
| Logs | `slog` `ReplaceAttr` redacts `Authorization`; never log tokens, bodies, or prompts | PII and secret leakage |
| Dependencies | `govulncheck` + CodeQL + gitleaks in CI | Known CVEs |

**Known gaps in this table:** no CSP is currently emitted, CORS defaults to `*` in dev, and
`sessions` have no rotation (see `refinementNotes` R20).

---

## 16. Performance Standards (miss = fail)

| Rule | Target |
|---|---|
| Static vs server | Client sites are static exports on Hostinger — zero Node per client. The proxy is the only server. |
| Latency | p95 `<100ms` on cache hit; p99 `<500ms` on an LLM call (the provider dominates). Read `duration_ms` from slog. |
| DB | `pgxpool` Min 2 / Max 20, `MaxConnLifetime 1h`; Supavisor 6543 TX mode at runtime, 5432 session for migrations. `EXPLAIN ANALYZE` the quota `SUM` query. |
| Redis | `io-threads 8`, `volatile-lru`/`allkeys-lru` with `maxmemory 75%`, and `HGETEX` for 1-RTT cache refresh. |
| Append-only tables | `usage`, `notify_jobs`, `agent_reminders` grow forever and are pruned by a janitor. Consider BRIN indexes on the timestamp column and monthly range partitioning so retention becomes `DROP PARTITION` rather than a mass `DELETE`. |
| Build | `-pgo=auto`; Swiss Tables + Green Tea GC are defaults — verify with `GOMEMLIMIT` under load. |
| Concurrency | Redis is the contention point before Postgres is. |
| Lighthouse | Not applicable (API only); client templates carry their own budget. |

---

## 17. LLM Cost Model and Prefix Caching

This section exists because it is the largest recurring-cost lever in the system and it is
currently unmeasured.

### 17.1 What the provider gives away

DeepSeek (the default `LLM_BASE_URL`) performs **automatic prefix caching**. The API returns
`usage.prompt_cache_hit_tokens` and `usage.prompt_cache_miss_tokens`, billed at a steep discount
versus uncached input — on the order of **~30x** for the V4 tier, with separate peak/off-peak
multipliers. *Verify the current rates against the provider's pricing page before modelling
anything; these move.*

Caching requires a **byte-identical prefix from token 0**. The provider persists a prefix unit at
request boundaries, on common-prefix detection, and at fixed token intervals; a later request that
replays a complete persisted unit is billed at the hit rate. Unused entries are typically evicted
within hours to days, so **hits are a measured optimization, never a correctness dependency** —
the system must be correct and affordable when every request misses.

### 17.2 What this service does today

Nothing. `internal/api/chat.go` and `internal/llm/client.go` declare only `PromptTokens` and
`CompletionTokens`; the cache fields are discarded at the decode boundary, and `usage.cost_usd`
therefore cannot tell a 30x-cheap input from an expensive one.

### 17.3 Why the prompt is already shaped correctly

Prefix caching is earned by prefix stability, and this system happens to have the right shape:

1. **Static per-tenant prefix.** `internal/agentconfig` loads a `SystemPrompt.md` (budgeted to
   8,000 runes), the tenant's skills, and its tool definitions. These are byte-stable per tenant
   and are prepended.
2. **Dynamic content last.** The per-request material — chat history, current user message — goes
   at the tail, which is the only position that does not invalidate the prefix.
3. **Frozen tool schemas.** Appending or reordering a tool invalidates the cached prefix from
   that point onward.

Measured provider behavior on a stable prefix is a 98–99% hit rate. **A single varying first line
drops it to 0%** — and it fails *silently*. No error, no premium, just a discount that never
arrives.

### 17.4 The rules

- **Never prepend a timestamp, request ID, chat ID, tenant lookup result, or session metadata.**
  Round the time to the hour or day if the model needs it, and put it at the end.
- **Treat the tool list as an ABI.** Adding a tool is a cache-busting event; expect one cold
  request afterward, not a regression.
- **Log `prompt_cache_hit_tokens` / `prompt_cache_miss_tokens`** and assert
  `hit + miss == prompt_tokens` — that identity is the API contract, and violating it means the
  parse is wrong.
- **Cost from a real price table** (hit / miss / output rates, time-of-day aware), not a fixed
  estimate.
- **This matters more here than in a typical chat app.** An agent tool loop re-sends the whole
  transcript plus tool results on *every* step, so prompt size grows with turn count. Prefix
  discipline is the difference between a flat and a superlinear per-conversation bill.

---

## 18. Protocol Posture: MCP (removed)

> **Status (2026-10-03):** `internal/mcp` is deleted and there is **no `/v1/mcp` route**.
> The paragraphs below describe the former posture and the upstream spec, kept so a future
> re-implementation does not repeat the same decisions.

**Upstream has since gone stateless.** The current spec revision (`2026-07-28`) removed
protocol-level sessions — no `Mcp-Session-Id`, no `initialize` handshake. Version and capabilities
now travel per request in `_meta` (`io.modelcontextprotocol/protocolVersion`); the GET/SSE
endpoint is replaced by `subscriptions/listen`; tasks moved to an official extension; SSE
resumability was removed; and `Mcp-Method` / `Mcp-Name` headers are required on POST. Older
revisions (`2025-03-26` … `2025-11-25`) used the sessioned shape the deleted code implemented.

**Posture on re-introduction:** no migration is urgent, but do not ignore it either.

- Implement **version negotiation** rather than a hardcoded shape: read the version, accept both,
  return an `UnsupportedProtocolVersionError` otherwise. The local Langchain bridge is the
  consumer to coordinate with.
- **Stateless does not mean unauthenticated.** Auth stays mandatory per request. Never bind a
  local MCP server to anything but loopback, and validate `Origin` to prevent DNS rebinding.
- Pin the protocol version explicitly in the client config so a library default bump cannot
  silently break the handshake.