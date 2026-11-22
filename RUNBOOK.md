# RUNBOOK — D:\MyProject\AgentsProxy

> Operations guide for the deployed agents-proxy. Pair with `README.md` (what it is) and `ARCHITECTURE.md` (how it's built). This file lives in the repo so any operator has context.

---

## 1. Health & Liveness

```bash
curl -s https://<host>/healthz
# {"status":"ok","version":"<sha>","db":true,"redis":true}
```

If `db` or `redis` is `false`, see "Service Down" below.

UptimeRobot should ping `GET /healthz` every 5 min and alert on 5xx.

---

## 1a. Agency auth, email, sessions

* **Sessions:** 30 days (`JWT_TTL_HOURS=720`). The cookie is refreshed when
  under 7 days remain, so active users stay signed in; idle >30 days is a
  clean redirect to login. Logout/revoke/password change bump `tokenVersion`.
* **Superadmin:** password + 6-digit email code. `ENV=production` with no
  Resend config fails closed (`OTP_UNAVAILABLE`) — password alone never logs in.
* **Client signup:** email + password + 6-digit code. Check
  `SIGNUP_ENABLED` and `GET /api/v1/auth/config`. The Resend sandbox sender
  (`onboarding@resend.dev`) can only mail the Resend account owner; verify
  `dfdagency.com` under Resend → Domains and set
  `EMAIL_FROM=DFD Agency <hello@dfdagency.com>` before enabling client signup.
* **Google:** CLIENT only. Staff emails are rejected by design (OTP stays
  mandatory). The button needs `GOOGLE_CLIENT_ID` (API) and
  `NEXT_PUBLIC_GOOGLE_CLIENT_ID` (web).
* **Orders:** `POST /api/v1/orders` requires a session; the order is bound to
  `orders.user_id` and appears in the portal immediately. Guests use WhatsApp.
* **Local code without Resend:** run with `ENV=development RESEND_API_KEY=`
  and read `dev verification code` from the server log.

---

## 2. Logs (slog JSON → stdout)

All logs are JSON on stdout. Production runs on Vercel:
`vercel logs https://agents-proxy-nine.vercel.app`.

Quick filters:

```bash
# All errors
vercel logs https://agents-proxy-nine.vercel.app | grep '"level":"ERROR"'

# All panics
vercel logs https://agents-proxy-nine.vercel.app | grep '"msg":"panic recovered"'

# Per-client traffic (uses client_id in attrs)
vercel logs https://agents-proxy-nine.vercel.app | jq 'select(.client_id=="<uuid>")'

# Per-request via request_id (returned in X-Request-ID)
vercel logs https://agents-proxy-nine.vercel.app | jq 'select(.request_id=="<id>")'
```

---

## 3. Common Incidents

### 3.1 `429` Spike

- **What it means:** clients hitting `quota` (monthly) or `ratelimit` (per-minute).
- **Diagnose:**
  ```bash
  # Top clients by 429s in last 1h
  vercel logs https://agents-proxy-nine.vercel.app | grep '"status":429' | jq -r .client_id | sort | uniq -c | sort -rn | head
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
  vercel logs https://agents-proxy-nine.vercel.app | grep '"msg":"listen"'  # confirm LLM_BASE_URL
  vercel logs https://agents-proxy-nine.vercel.app | grep '"status":502'    # count of 502s
  ```
- **Fix:**
  - Provider outage: wait + `POST /v1/admin/clients` is unaffected. Chat returns 502 to the client widget; user sees a graceful error.
  - Bad `LLM_MODEL`: update `LLM_MODEL` env, then redeploy (Vercel → Deployments → Redeploy).
  - Bad `LLM_BASE_URL`: same — fix env, then redeploy.

### 3.3 Postgres Down

- `GET /healthz` returns 500 with `db:false`.
- Diagnose from a shell with an ad-hoc admin DSN (from the password manager,
  never stored in `.env`):
  ```bash
  psql "$DATABASE_ADMIN_URL" -c 'SELECT 1'
  ```
- Fix: see Supabase status page (prod) or `laragon start` (dev).

### 3.4 Redis Down

- `GET /healthz` returns 500 with `redis:false`.
- Cache misses fall back to DB. `rate-limit` middleware fails-open (allows the request) — this is by design, see `middleware/ratelimit.go`. So Redis being down means loss of caching only, not a hard outage.
- Fix: restart Redis. Verify with `redis-cli -h <host> ping`.

### 3.5 Sentry Not Receiving Events

- Check `SENTRY_DSN` is set in the Vercel project environment.
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

# 3. Wait for CI to pass; Vercel deploys main automatically.
#    For an immediate binary rollback, promote the last known-good
#    production deployment (Vercel dashboard → Deployments → Promote, or):
vercel ls agents-proxy
vercel rollback <known-good-deployment-url> --yes
```

Database migrations are additive and forward-only. Rolling the binary back
does not roll the schema back; the previous binary must tolerate the newer
schema (this is why additive migrations are mandatory). A destructive change
needs a separate phased migration, not a revert.

**Schema-clone contract:** after migrations 079/080 and Centrum 0005, the
database no longer has `clients.project_key` or
`project_centrum.sports_sessions`. Do not promote a binary older than the
schema-clone release: it writes `project_key` on admin create and cleans the
legacy session table. Roll back to a binary from this release line, or
re-add the column temporarily with a deliberate compatibility migration.

For data incidents: restore from `ops/backup.sh` output:
```bash
gunzip -c backups/agents_proxy-2026-08-31T17-00-00Z.sql.gz | psql "$DATABASE_ADMIN_URL"
```
Supply `DATABASE_ADMIN_URL` ad hoc from the password manager. It must not
live in a local `.env`; the app refuses a remote database in development.

---

## 5. Environment variables

| Env | Default | Notes |
|---|---|---|
| `PORT` | 8080 | |
| `ENV` | development | strict enum: `development` / `staging` / `production`; anything else fails boot |
| `DATABASE_URL` | required | runtime pool — **must be `app_runtime` (NOBYPASSRLS) in production**; boot rejects superuser/BYPASSRLS |
| `DATABASE_ADMIN_URL` | empty | migrations + admin endpoints — superuser (Supabase 5432 direct); required when `ENV=production` |
| `EXPECTED_DB_REF` | empty | optional Supabase project ref guard; when set, both DSNs must belong to that project or boot fails |
| `ALLOW_REMOTE_DB` | false | staging-only opt-in for a remote database; development always rejects remote |
| `RUN_MIGRATIONS` | true in development, false otherwise | Vercel preview/production may never set true |
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
| `TEST_DATABASE_URL` | empty | tests only; local throwaway DB as `app_runtime` |
| `TEST_DATABASE_ADMIN_URL` | empty | tests only; local throwaway DB as `postgres` |
| `TEST_REDIS_URL` | empty | tests only; local Redis |

### 5a. Environment safety (enforced before any DB connection)

* `ENV=development` + any non-loopback database host → boot fails. A
  production DSN on a laptop cannot be used, even with
  `ALLOW_REMOTE_DB=true`.
* `ENV=staging` + remote host requires `ALLOW_REMOTE_DB=true`.
* `EXPECTED_DB_REF` (production and staging) makes the remote-host rule
  identity-based: the pooler username (`role.<ref>`) or direct host
  (`db.<ref>.supabase.co`) must contain the configured ref. This is what
  stops a copied staging DSN from booting production and vice versa.
* `VERCEL_ENV=production` requires `ENV=production`; `VERCEL_ENV=preview`
  rejects `ENV=production`.
* `RUN_MIGRATIONS=true` on Vercel preview/production → boot fails.
* `ENV=production` + a runtime role with `rolsuper` or `rolbypassrls` →
  boot fails. The log line is `runtime role verified role=app_runtime`.
* Vercel Preview must have **no** production variables. A backend preview
  fails with `DATABASE_URL is required`; that is the intended fail-closed
  state until the staging project exists.

### 5b. Secret rotation

Generate URL-safe values (`openssl rand -hex 24`), store them in the password
manager first, then rotate in this order. Never paste a secret into chat or a
shell history.

| Role | Used by | Rotate | Update | Verify |
|---|---|---|---|---|
| `app_runtime` | Vercel Production `DATABASE_URL` | `ALTER ROLE` in Supabase SQL editor | Vercel Production env, then redeploy | `/healthz` + one tenant read |
| `postgres` | Vercel Production `DATABASE_ADMIN_URL` | Supabase dashboard → **Reset database password** (not raw SQL; pg_net/internal services) | Vercel Production env, then redeploy | `go run ./cmd/migrate` (no-op) + admin read |
| `backup_reader` | GitHub `BACKUP_DATABASE_URL` | `ALTER ROLE` | GitHub secret | run the backup workflow |
| `delivery_caller` | Edge Function `DELIVERY_DATABASE_URL` | `ALTER ROLE` | Supabase Edge Function secret | next reminder tick / function log |

Existing pooled sessions survive a password change; new connections fail
until the consumer has the new value. Do `postgres` last, in a low-traffic
window.

Supavisor's **session** pooler (port 5432) can reject a just-changed password
for roughly 10–20 seconds while it refreshes its credential cache. The
transaction pooler (6543) often accepts it immediately. Wait and retry the
5432 check before concluding the rotation failed; do not rotate again on the
first auth error. Session mode is required for `pg_dump`, so the backup
credential must be verified on 5432, not 6543.

Both `backup_reader` and `delivery_caller` were rotated and verified in
November 2026. Their current values live only in the GitHub secret and the
Supabase Edge secret; if either is lost, rotate again rather than trying to
recover it from logs or a deployment.

## 6. Feature toggle (instant or ≤15 min)

`PATCH /v1/admin/clients/{id} {"features":["records","menu","booking"]}` — the cache entry is evicted on the way out, so the next request sees the new feature set. If Redis is down, the cached entry lives up to 15 minutes (the cache TTL); use `redis-cli DEL ck:<sha256>` to force the eviction.

## 7. Escape-hatch decision rule

A request is **in-request** when:
- it finishes in <300ms, AND
- it touches no third-party API that needs a secret, AND
- it does not need to write to a non-proxied storage (e.g. an external billing API).

Otherwise: **Edge Function** (DFD Supabase Edge Functions or a Hostinger serverless endpoint). The proxy stays the canonical path for LLM, cache, and tenant CRUD; the Edge Function handles the side trip.

---

## 8. Client Onboarding (DFD agent flow)

```bash
ADMIN="Authorization: Bearer $ADMIN_API_KEY"

# 1. Create client (one-time per client)
curl -s -X POST https://<host>/v1/admin/clients \
  -H "$ADMIN" -H "Content-Type: application/json" \
  -d '{"name":"warung-sari","tenant_id":"warung-sari","vertical":"fnb","schema":"project_warung_sari","monthly_quota_tokens":500000}'
# -> returns {id, name, tenant_id, token, schema, monthly_quota_tokens}
#    The token is shown ONCE. Save it now.
#    A project_* schema requires projects/<slug>/project.json and a clean
#    project ledger (run migrate first). Omit schema for a platform tenant;
#    it goes to public. Every project, including centrum, uses project_<slug>.

# 2. Inject into the new frontend project:
#    NEXT_PUBLIC_AGENTS_PROXY_URL=https://<host>
#    NEXT_PUBLIC_AGENTS_PROXY_TOKEN=<token from step 1>
#    NEXT_PUBLIC_AGENTS_PROXY_TENANT=warung-sari

# 3. Verify (new projects use the project-scoped prefix)
curl -s https://<host>/v1/p/warung-sari/records -H "X-Client-Token: <token>"
# -> 200, items: []  (empty until first POST)

# 4. Suspend / resume
curl -X PATCH https://<host>/v1/admin/clients/<id> \
  -H "$ADMIN" -H "Content-Type: application/json" \
  -d '{"status":"paused"}'
```

A project tenant is born in this order. The folder and its schema must exist
before the client row does; the admin API never creates DDL.

```bash
# 1. Add projects/<slug>/project.json and migrations/, deploy the build.
# 2. Apply platform + project migrations (creates the schema and ledger).
go run ./cmd/migrate

# 3. One audited command: validate, create the client, seed the first owner.
export AGENTS_PROXY_URL=https://<host>
export ADMIN_API_KEY=...                    # from the password manager
export DATABASE_ADMIN_URL=...               # only for the migrate step

go run ./cmd/provision --slug warung-sari --name "Warung Sari" \
  --owner-email owner@warung-sari.example --generate-owner-password
```

`cmd/provision` prints the client token and the generated owner password
once. It pauses the client if owner seeding fails, so an active ownerless
tenant is not left behind. A platform tenant (no project folder, shared
`public` schema) uses `--platform`:

```bash
go run ./cmd/provision --platform --slug mail --name "Mail"
```

A platform tenant has no project staff provider, so `cmd/provision` rejects
`--owner-email` for it; there is no project user table to seed.

The lower-level path is still available:

```bash
go run ./cmd/tenant create --slug warung-sari --name "Warung Sari"
go run ./cmd/tenant list
go run ./cmd/tenant patch --tenant warung-sari --status paused
go run ./cmd/tenant usage --tenant warung-sari
```

The admin API rejects a `project_*` schema unless the folder exists, the
folder's `project.json` matches the schema, and the project ledger is
complete. `--platform` selects `public`; a project slug with a folder cannot
be created as a platform tenant.

If the project uses the generic records feature, its `0001_init.sql` must
create `tenant_records` in the project schema. If it has a custom agent, add
`projects/<slug>/agent/` and run `go generate ./...` before deploying.
`project.json.features` is the default entitlement for a new tenant; the
compiled project registry must also know each name.

A project's business tables live in its own schema; its frontend uses
`/v1/p/<slug>/...`. `/v1/{vertical}` is platform-only. Centrum's frontend is
on `/v1/p/centrum/...`, and the old `/v1/sports/...` alias is gone.

---

## 8a. Tenant hygiene

Production is intentionally single-tenant for the client plane: `centrum`.
The paused `dfd-agency` row is retained only because its `frontend_origins`
entry powers CORS for the live DFD Agency site.

Demo/test tenants left behind by local integration runs are removed with:

```bash
psql "$DATABASE_ADMIN_URL" -f scripts/prune-demo-tenants.sql
```

The script keeps `centrum` and `dfd-agency`, deletes every other
`tenant_id` from every table in `public` and every `project_*` schema
(including orphan rows such as `tool-test`), then deletes the extra
`clients` rows and their `project_migrations` entries. It does **not** drop
project schemas; decommissioning a project is a separate export-then-drop
step.

Never run the DB-backed integration suite against production. Tests read
`TEST_DATABASE_*` only and `make test` refuses a remote host; CI uses a
local Postgres service.

---

## 9. Schema Migrations

`go run ./cmd/migrate` applies:
1. platform migrations in `internal/db/migrations/` (public schema), then
2. each project's `projects/<slug>/migrations/*.sql` (its own schema).

On Vercel `RUN_MIGRATIONS=false` (and is forced false): cold starts must not
replay the ledger, and a preview must never run DDL. Production migrations run
through the manual `db-migrate` GitHub workflow, gated by the `production`
Environment; it always checks out `main` and runs the same command against the
admin DSN. The Environment is restricted to protected branches (main).
GitHub Free does not support required reviewers; on a plan that does, add
them. The workflow itself also refuses to run from any ref other than main.
`PROD_DATABASE_ADMIN_URL` is still a repository secret because the same
read-only secret is used by `schema-check`; move it to the production
Environment secret when the plan supports environment-scoped secrets and
update both workflows.

**Release order matters.** Merge a migration-only PR to `main`, run
`db-migrate` with `apply`, then merge the code that depends on the new
schema. Additive, backward-compatible migrations may ship with their code,
but the binary must never require a column the database does not have yet.

The schema-clone/auth release follows the expand/contract order:

1. Apply `077`–`079` and Centrum `0004`/`0006`. The auth functions keep the
   same signatures, so the old binary stays compatible; sessions are copied
   into `public.staff_sessions`.
2. Deploy the binary. It stops writing `clients.project_key`, uses
   `public.staff_sessions` in the janitor, and resolves the tenant schema for
   auth and money summaries.
3. Apply `080` and Centrum `0005`. They remove the dead `project_key` column
   and the legacy `project_centrum.sports_sessions` table. Do this only after
   the new binary is live.

The staff-auth ownership release is additive first:

1. Apply `081_staff_sessions_generic.sql`. It adds
   `private.staff_session_create(tenant, user, hash, expiry)` and
   `private.staff_session_resolve(hash)`. The old `login_user` /
   `create_session` / `session_user` functions stay, so the previous binary
   keeps working and live sessions survive.
2. Deploy the binary. `internal/authapi` now uses the provider registry; the
   Centrum provider reads `sports_users` through `tenantdb` with RLS. Login no
   longer scans every project.
3. After the stability window, apply Centrum `0010_contract_legacy_auth.sql`.
   It drops the three old functions, revokes `auth_definer`'s
   `project_centrum` access, and grants only `clients (tenant_id, status)`.
   Keep those two columns: `staff_session_create` still validates that the
   tenant is active. Do this only when no pre-081 binary can still serve
   traffic.

The project-data contract follows the same pattern. Centrum `0007`/`0008`
move staff payments and the money summary into the project schema; deploy the
binary first, then apply:

1. `0009_contract_staff_payments.sql` copies any rows written to
   `public.agent_staff_payments` after `0007` into
   `project_centrum.sports_staff_payments`, verifies none are missing, then
   drops the public table.
2. `0011_contract_range_summary.sql` drops `private.range_summary`; it then
   retires `money_reader` with `DROP OWNED BY money_reader` +
   `DROP ROLE money_reader`, guarded so an unexpected owned object aborts the
   migration instead of being silently dropped.
3. `migrate check` must stay clean; the daily `schema-check` is the proof.

The contract migrations are deliberately at the end of Centrum's project
chain, not in the platform chain. `MigrateAll` applies platform migrations
first, and Centrum `0004`/`0006` recreate the legacy auth functions, grants,
and role. Cleanup has to run after `0008` so a fresh database reaches the same
final state as an upgraded one.

**Route contract.** The project-owned vertical alias is retired. Centrum is
reachable only under `/v1/p/centrum/...`; `/v1/{vertical}` mounts platform
tenants only. The `LEGACY_PROJECT_ROUTES` flag, `RegisterLegacyProject`, and
the frontend `NEXT_PUBLIC_PROXY_PREFIX` rollback switch are removed. Verify
`/v1/sports/facilities` is 404 and `/v1/p/centrum/facilities` is 200 after a
deploy.

Both ledgers store a SHA-256 checksum. An applied file that changed after
application fails the migration with a clear error; add a new migration
instead. Platform rows are in `public.schema_migrations`; project rows are in
`public.project_migrations` (scoped by `project_slug`) and now carry
`db_schema`, the schema the version was applied to. A project that changes
its `project.json` schema fails loudly instead of skipping every recorded
migration in a new, empty schema.

### Inspecting without writing

The same command has three read-only modes. They run inside a `READ ONLY`
transaction and never create the ledger tables, so they are safe against
production:

```bash
go run ./cmd/migrate status   # every file: applied, pending, drift, missing
go run ./cmd/migrate check    # exit 1 unless everything is applied and unchanged
go run ./cmd/migrate plan     # only what apply would still run
```

Add `--json` for CI. The `db-migrate` workflow exposes the same modes through
its `command` input (`apply` is the default). `check` also fails when:

* `clients.db_schema` disagrees with a project's `project.json`;
* the partial unique index `clients_project_schema_key` is missing;
* two projects claim the same slug or schema, or two clients share a
  `project_*` schema;
* a client points at a `project_*` schema that no project folder declares;
* the ledger contains a project folder that disappeared from the build;
* a table in any project schema is missing `ENABLE+FORCE RLS`.

The `schema-check` workflow runs the same read-only check daily. Tests use
`internal/dbtest` to clone a tenant schema; no feature test may query
`project_centrum` directly (`scripts/lint-tests.sh`).

The runner takes `pg_advisory_xact_lock(734561)` for platform migrations and
`734562` per project. On Vercel, `RUN_MIGRATIONS=false`; production applies
DDL only through the workflow above.

To add a platform migration: drop `internal/db/migrations/0XX_name.sql`.
To add a project migration: drop `projects/<slug>/migrations/000X_name.sql`
and set that project's `schema` in `project.json`. Sort order is lexical.
Project schemas must match `project_<slug>`; `public` is reserved for the
legacy platform tenant (dfd-agency), which owns no project folder.

Production and staging run PostgreSQL 17. CI and `docker-compose.yml` use
`postgres:17-alpine` so a migration cannot pass on an older server and fail
in production.

**Phase 4 note:** migrations `007`–`010` (sports vertical) create custom tables with RLS. Migration `008` runs `CREATE EXTENSION btree_gist` (needs superuser) — migrations run on the admin pool, so this is fine. The booking exclusion constraint (`tsrange`, NOT `tstzrange` — see ARCHITECTURE §11.3) prevents double-bookings at the DB level; a concurrent double-book surfaces as HTTP `409 slot already booked`.

---

## 9a. Backup and recovery

`backup.yml` runs every 6 hours (01:00/07:00/13:00/19:00 Asia/Jakarta),
dumps through `ops/backup.sh`, and restores the dump into a scratch
`postgres:17` in the same workflow. Artifacts are retained for 30 days.

* **RPO:** up to 6 hours. The free Supabase plan has no PITR
  (`pitr_enabled=false`); the logical dump is the recovery point.
* **RTO:** restore the newest artifact, then recreate roles/extensions and
  apply the embedded migration chain. The restore smoke already exercises
  the schema + data path; treat a failed restore-smoke run as a release
  blocker.
* **Scope:** the dump includes platform and project schemas. Roles are
  cluster-level and are recreated by the platform migration chain; hosted
  extensions (`pg_cron`, `pg_net`) are recreated by the host.

Do not restore production data into staging. Staging is synthetic.

---

## 10. Performance & Capacity

- **Latency targets:** p95 `<100ms` on cache hit, p99 `<500ms` on LLM call (provider-dominated).
- **DB pool:** `MinConns 2, MaxConns 20` per replica. If you see `conn busy` in logs, scale horizontally.
- **Redis hot keys:** `ck:{hash}` (15m TTL) + `rl:{id}:{min}` (60s TTL) + `quota:{id}:{month}` (30s TTL). On Redis 8 with `allkeys-lru`, 75% `maxmemory` is the recommended ceiling.

---

## 11. Security Audit Checklist

- [x] `subtle.ConstantTimeCompare` on token + admin key (auth/tokens.go)
- [x] `pgx` parameterized (`$1,$2`) — no `fmt.Sprintf` into SQL anywhere
- [x] RLS on `tenant_records` with `FORCE ROW LEVEL SECURITY` + RESTRICTIVE-style predicate (migration 002)
- [x] Every project table `ENABLE+FORCE RLS`, audited by `migrate check`
- [x] `clients.db_schema` constrained to `public` or `project_*`; partial unique index prevents duplicate ownership
- [x] Staging/production DB identity pinned with `EXPECTED_DB_REF`
- [x] slog redaction: `Authorization` + any `*password*` / `*secret*` keys → `[REDACTED]`
- [x] Sentry BeforeSend: same redaction
- [x] Rate limit + quota
- [x] `Server{ReadHeaderTimeout: 5s, ReadTimeout: 10s, IdleTimeout: 120s}`
- [x] `govulncheck` in CI

---

## 12. When You Need to Change Things

| You want to… | Touch |
|---|---|
| Add a new vertical | `internal/features/builder.go` — append one string to `AllowedVerticals` |
| Add a new business field | nothing — it's all in `payload` jsonb |
| Add a new admin endpoint | `internal/api/admin.go` (proxy) or `internal/agency/admin/` (agency plane) |
| Add an agency endpoint | handler in `internal/agency/<domain>/` + route in its `RegisterRoutes` (chi) |
| Change rate limit | `RATE_LIMIT_PER_MINUTE` env (proxy); `ratelimit.NewRateLimiter` in `internal/agency/router.go` (agency writes) |
| Add a Sentry env var | `internal/config/config.go` + `internal/sentry/sentry.go` |
| Add a migration | new file in `internal/db/migrations/`, never edit applied ones |
| Change the LLM provider | `LLM_BASE_URL` + `LLM_API_KEY` + `LLM_MODEL` env |
