# Architecture hardening — November 2026

Status: **P0/P1/P2, security-polish, and exit-gate controls are implemented; A-10 is live and verified in production. Remaining accepted risks and operational gaps are marked.** This document is the single tracker for the four-audit review (routing/layering, database/RLS, auth/isolation, docs/ops/CI). An item is only marked `[x]` when the code is merged and the named verification passes.

Legend: `[x]` implemented and verified · `[~]` partially implemented /
external decision pending · `[ ]` open · `[-]` accepted residual, documented.

---

## P0 — correctness, safety, security

- [x] **P0-1 Platform route contract.**
  `/v1/{vertical}` is mounted only when a live platform tenant of that
  vertical exists at boot. The platform identity middleware accepts any
  valid client token and `/records` is ungated, so a project tenant with
  the same vertical can use the platform surface. A newly created
  platform vertical is 404 until restart.
  Fix: mount every allowed platform vertical statically and require
  `clients.db_schema = 'public'` after identity. Verify a project token
  is rejected on a mounted platform vertical.

- [x] **P0-2 Staff role revocation.**
  `UserByToken` caches the full user (including `role`) for 60s with no
  invalidation. A demoted/deactivated owner can re-promote themselves or
  mint a new owner through `/v1/auth/register` while the cache is warm.
  Fix: resolve the live profile on every request (remove the full-user
  cache), so role/is_active/tenant-status changes take effect on the next
  request. Verify demotion and deactivation immediately revoke access.

- [x] **P0-3 Agency grants on the runtime role.**
  `app_runtime` has full DML on agency tables (`users`, `api_keys`,
  `login_otps`, `signup_requests`, orders, payments, audit logs, webhook
  tables), while the agency router runs on `adminPool`. Fix: revoke all
  agency table/sequence privileges from `app_runtime`; pin the absence
  with a test.

- [x] **P0-4 Restorable backups.**
  `ops/backup.sh` dumps with `--no-owner --no-privileges`, so a restore
  has no grants or definer ownership; the restored ledger makes
  `migrate apply` a no-op; the smoke connects as `postgres`, so it passes
  on an unusable restore. The script also echoes the read-everything DSN
  and writes a world-readable dump. Fix: include ownership/ACLs, restore
  the required roles first, run the smoke as `app_runtime`, assert
  non-zero platform and project rows, redact the DSN, use `umask 077`.

- [x] **P0-5 Test-harness safety.**
  `scripts/test-local.sh` validates only `TEST_DATABASE_URL`. A stale
  `TEST_DATABASE_ADMIN_URL` or `TEST_REDIS_URL` can point at production
  while the guard passes; the API tests then delete clients and
  `FlushDB`. Fix: require and loopback-validate all three, and refuse
  destructive test setup when any target is not local.

- [x] **P0-6 Workflow secret exposure.**
  `backup.yml` and `schema-check.yml` accept `workflow_dispatch` from any
  branch with production secrets and check out that ref. Fix: require
  `refs/heads/main`, check out `main`, and use the `production`
  environment.

---

## P1 — architecture invariants

- [x] **P1-1 Tenant transaction boundary.**
  `tenantdb.WithTx` falls back to `public` for an unknown tenant; the
  records/feature-adapter queries have no explicit `tenant_id`; a
  project URL is not bound to the project schema. Fix: fail closed on an
  unknown tenant, add explicit tenant predicates, and pin the project
  schema in the project router. Run the runtime-role check in staging
  too.

- [x] **P1-2 RLS regressions and honesty.**
  `063` re-added the blanket `usage_read_runtime` policy that `053`
  deliberately dropped; `usage` INSERT was `WITH CHECK (true)`; ADK
  session tables used `USING (true)` with no tenant predicate. Fix:
  drop the usage read policy, bind the usage insert to `app.client_id`
  (086), and tenant-scope the ADK tables on `app_name = app.tenant_id`
  (085). The session service sets the GUC in every transaction.

- [x] **P1-3 One project manifest.**
  `agentconfig`, `db/project_migrations.go`, and `cmd/genprojects` apply
  different rules; `project_<slug>` is only enforced as `project_*`;
  feature names and verticals are not validated at registration;
  duplicate project identity is not caught by the router loader. Fix:
  centralize manifest loading/validation, enforce exact schema/slug/
  vertical/feature invariants, validate `project.json.features` against
  the compiled registry, and fence a bad project with 503 or fail boot.

- [x] **P1-4 Server-derived agent vertical.**
  `/v1/agent` accepts `vertical` from the request and defaults to
  `sports`. Fix: derive it from the tenant/project config; ignore request
  input.

- [x] **P1-5 Migration integrity.**
  `migrate check` does not prove object presence (an empty recreated
  schema can be clean); project apply ignores orphan migration files;
  platform and project runners use separate advisory locks. Fix: add an
  object-presence check, reject orphan project files on apply, and
  serialize the whole `MigrateAll`. The backup script refuses a role
  without `BYPASSRLS` (Supabase's `postgres` cannot alter that attribute,
  so this is checked at use rather than in a migration).

- [x] **P1-6 Docker/release build.**
  `.dockerignore` excludes `*.md`, but `projectembed.go` embeds
  `projects/*/agent/agent.md` and skills. `make docker` and
  `release.yml` cannot build. Fix `.dockerignore` and add a Docker build
  to CI. The CI Docker job passed (run 45075744093).

- [x] **P1-7 Production config guards.**
  `DB_POOL_MIN=0` is rejected and silently becomes 2; Vercel-only
  requirements are not validated; `ADMIN_API_KEY`/`JWT_SECRET` accept
  placeholders/short values; `EXPECTED_DB_REF` is optional;
  `.env.staging.example` uses the `postgres` DSN. Fix the parser, add
  Vercel/server secret validation, require the ref in staging/
  production, and correct the staging example.

- [x] **P1-8 One production DDL path.**
  `cmd/provision` runs `db.MigrateAll` by default and bypasses
  `internal/config`'s environment/ref guards. Fix: make migration
  explicit and guarded; the deploy workflow remains the only production
  DDL entry point.

---

## P2 — clean final state

- [x] **P2-1** `README.md` no longer advertises `/v1/sports`.
- [x] **P2-2** Rollback boundary covers Centrum `0009`–`0011`.
- [x] **P2-3** Backup comments say 2-hourly, not nightly/unscheduled.
- [x] **P2-4** `ops/schedule-delivery.sql` is clearly historical or removed.
- [x] **P2-5** Dead scripts (`seed_centrum_exhaustive.mjs`, stale
      `setup-staging.sh`) removed or fixed; `seed-dev.mjs` verification
      uses the project route.
- [x] **P2-6** Duplicate route tests and global-registry test pollution
      removed.
- [x] **P2-7** Stale comments/paths fixed (`ProjectVertical` doc,
      `definers_test`, `ARCHITECTURE` pool/`additive-only` wording,
      generated-file path).
- [x] **P2-8** `Deps.AdminKey` removed; `RequireAdmin` rejects an empty
      configured key; `RequireFeature` does not bypass for `public`.

---

## Architecture closure — post-release hardening

- [x] **A-1 Platform vertical pin.** `RequirePlatformVertical` now requires
  the resolved tenant's `vertical` to equal the mounted `/v1/{vertical}`
  surface, so a platform tenant with one vertical cannot use another
  vertical's records routes.
- [x] **A-2 Uniform feature entitlement.** Public landing reads and staff
  sessions are checked against the tenant's live `clients.features`; the
  `PublicRead` marker now only removes authentication, never entitlement.
- [x] **A-3 Project manifest immutability.** Admin `PATCH /clients/{id}`
  rejects a project vertical change; a vertical change on a platform tenant
  revalidates the effective feature set.
- [x] **A-4 One IP trust boundary.** Platform rate limits, public history,
  agency limits, and audit rows use `internal/requestip`; the agency limiter
  moved from per-process memory to Redis and no longer trusts a raw
  `X-Forwarded-For`.
- [x] **A-5 Secure agency cookies.** Staging and Vercel preview set the
  `Secure` flag; only an explicitly local development process may omit it.
- [x] **A-6 One project catalog.** Boot loads one manifest snapshot, validates
  project features against the compiled registry, and passes the same map to
  the REST router, the agent loader, and the admin provisioning catalog.
  Production no longer mounts a project from the compiled registry without a
  folder; boot and `migrate check` reject a compiled project whose
  `project.json` is missing.
- [x] **A-7 Project hooks without platform coupling.** Booking notifications
  go through `internal/projecthooks`; `cmd/server` no longer imports a
  Centrum package.
- [x] **A-8 ADK cache lock safety.** Tool-set builds no longer hold the global
  cache lock across MCP network calls, use the caller's context, and use a
  generation counter so an in-flight build cannot repopulate after
  `UpdateProject`.
- [x] **A-9 Public health detail.** `/healthz` reports only an aggregate
  fence count; tenant slugs and internal reasons stay on the admin endpoint.
- [x] **A-10 Deploy-order hard gate.** `deploy.yml` runs `cmd/migrate check`
  against production before building, stages a production Vercel deployment
  with `--skip-domain`, runs `cmd/smoke` on the staged URL, promotes it via
  the project-scoped Vercel REST API, and runs the same contract on the alias.
  A post-promotion failure promotes the captured previous deployment back.
  Vercel Git auto-deploy for `main` is disabled in `vercel.json`; PR previews
  remain automatic. The deploy workflow never applies DDL. **Live rollout
  verified on `dbf8225` (deploy run 45439936391).**
- [x] **A-11 Shared entitlement validator + boot audit.** `cmd/migrate`,
  boot, and `tenant-reload` use `features.ValidateClientEntitlement`. Boot
  fences active project tenants with empty/unknown/vertical-mismatched
  entitlements, persists the reason, keeps healthy tenants serving, and
  reconciles stale boot/reload fences. Reload refuses to release an
  entitlement fence until the live `clients` row is fixed.
- [x] **A-12 Fresh test database per run.** `cmd/testdb` creates
  `agents_proxy_test_*`, `scripts/test-local.sh` applies the chain, runs the
  suite, and drops the database with `FORCE`. Redis DB 15 is reserved for
  tests. CI uses the same runner. This removes the stale `project_test_*`
  schema failure mode.
- [x] **A-13 Performance baseline.** `cmd/bench`, `cmd/benchseed`, and
  `scripts/bench.sh` produce per-scenario p50/p90/p95/p99, throughput,
  status/error distributions, pprof, Redis stats, and hot-path `EXPLAIN`
  plans. `docs/PERF-BASELINE-2026-11.md` records the first baseline and the
  top five bottlenecks.
- [x] **A-14 Strict feature rollout.** `project.json` can set
  `strict_features: true`, which makes its `features` list a minimum
  entitlement. `cmd/migrate check`, boot audit, and tenant reload then fail
  if an existing tenant is missing a declared feature. Centrum opts in, so a
  newly added feature cannot ship without its clients row being backfilled.
- [x] **A-15 Free-plan operations guards.** Backup has two two-hourly cron
  offsets, smoke auto-triggers a replacement backup when the latest success
  is over four hours old, and failed smoke/perf probes open or update a
  single GitHub issue. A daily `perf-guard` workflow enforces runner-specific
  public regression ceilings at a rate-limit-safe 0.2 RPS; the absolute
  same-region SLOs remain the baseline harness. PITR remains intentionally
  disabled on the free plan.
- [x] **A-16 Supply chain.** GitHub Actions are pinned to commit SHAs with
  version comments, `govulncheck` is pinned, and Dependabot covers Go modules
  and GitHub Actions. Vercel CLI remains pinned to `66.6.0`.

---

## Security polish — post-hardening controls

These are additive controls on top of the P0/P1/P2 invariants. They are
marked `[x]` once the implementation and named tests pass; rollout evidence
is recorded in the verification log.

- [x] **S-1 Staff-login brute-force protection.**
  `/v1/auth/login` has a per-tenant/email attempt bucket (default 10/min)
  and a failure lockout (default 5 failures / 15 min) in Redis. Keys are
  SHA-256 hashes of tenant+email, so Redis never holds a raw address.
  Unknown tenants/users still pay a bcrypt comparison, keeping the failure
  path timing-comparable. A locked account returns 429 + `Retry-After`;
  a successful login clears the failure budget. Redis errors fail open
  (availability) with a warning. Tuning:
  `LOGIN_RATE_LIMIT_PER_MINUTE`, `LOGIN_LOCKOUT_FAILURES`,
  `LOGIN_LOCKOUT_MINUTES`.

- [x] **S-2 Push endpoint SSRF allowlist.**
  `internal/pushsafe` accepts only HTTPS endpoints on
  `fcm.googleapis.com`, `web.push.apple.com`,
  `updates.push.services.mozilla.com`, `push.services.mozilla.com`, and
  `*.notify.windows.com` (dot boundary), on port 443, with no userinfo and
  a non-empty path. Validation runs on subscribe and again immediately
  before every send; invalid stored rows are pruned and never dialed. The
  sender's HTTP client also refuses redirects outside the allowlist, and
  logs only the endpoint host. Tests:
  `TestValidPushKeys_ServiceAllowlist`,
  `TestPushSender_RejectsDisallowedEndpoint`, and the `pushsafe` table
  tests.

- [x] **S-3 Admin audit trail.**
  Migration `087_admin_audit_log.sql` adds an append-only table for the
  `/v1/admin` surface. The middleware inserts a row BEFORE the handler
  (fail closed on insert failure), then records status and duration. Actor
  is a 12-hex SHA-256 fingerprint of the configured `ADMIN_API_KEY`, never
  the key. Safe target fields (`id`, `tenant_id`, `origin`, `suffix`,
  `feature`) are captured from URL/query/body; request bodies are never
  stored, so the owner password cannot leak into the trail. RLS is enabled
  with no policy; `app_runtime`/`anon`/`authenticated` have no grant, pinned
  by `TestAgencyTablesAreNotRuntimeAccessible`. Test:
  `TestAdminAuditWritesRow`.

- [x] **S-4 Report-capability log redaction.**
  `/v1/reports/{token}` and `/v1/reports/jobs/{id}` authenticate by path
  possession. `middleware.RedactPath` replaces the capability segment in
  access logs, Sentry breadcrumbs, and panic reports with `[REDACTED]`.
  Report-job build logs use a short fingerprint instead of the UUID.
  Tests: `TestRedactPath`, `TestFingerprint`.

---

## Remaining ops backlog (separate from architecture)

- [x] Centrum `next`/`eslint-config-next` → 16.4.6; Dependabot #16 closed.
  `npm audit --omit=dev` is clean, `npm run check` is 14 files/159 tests, and
  `npm run build` passes.
- [~] Backup RPO: the logical dump is now scheduled every 2 hours
  (best-effort), down from 6 hours. Restore correctness is fixed and the
  drill is green. The 30-minute smoke workflow also fails if the latest
  successful backup is older than 4 hours, so a skipped dump becomes
  visible before the window widens much further. Supabase PITR is available
  as a paid add-on ($100/mo for 7-day retention) but is not enabled.
  GitHub's scheduler can delay or skip runs, so a guaranteed 2-hour RPO
  still needs PITR or an external scheduler; do not claim the guarantee
  from the cron alone.
- [~] DFD origin: the shared `client/api-origin.mjs` change is in place and
  the production pin/preview fail-closed behavior is unchanged. DNS check:
  `dfdagency.com` uses Afternic nameservers (parked), `api.dfdagency.com`
  has no usable A/CNAME/TLS record, and the domain is not in the Vercel
  account. Adding the custom domain needs domain-owner/DNS action, not a
  code change.
- [-] GitHub reviewer: accepted risk for now. The repository is private on
  Free and has one maintainer; the enforced controls are the three required
  checks, strict branch updates, and `enforce_admins`. A required reviewer
  needs GitHub Team plus a second trusted person, otherwise it locks out the
  only maintainer.

These are tracked in `RUNBOOK.md` and are not blockers for the
architecture work above.

---

## Verification log

- `go test -p 1 ./internal/features ./internal/db` — pass
  (contract test proves the agency tables are no longer reachable by
  `app_runtime`; `usage_read_runtime` absence and the RLS behavior test pass).
- `go test ./internal/api -run 'TestPlatformSurfaceRejectsProjectTenant|TestProjectRouteRejectsWrongSchema|TestCentrumProjectRoutes'` — pass.
- `go test ./internal/authapi -run 'TestUserByTokenReadsLiveRole|TestLoginMeLogout'` — pass.
- `go test -p 1 ./internal/tenantdb ./internal/features/shared/records` — pass.
- `go test ./internal/config ./internal/agentconfig ./internal/features ./cmd/provision` — pass.
- `go run ./cmd/migrate check` against the local database — `migrations clean`.
- Local backup/restore drill: `ops/backup.sh` produced a 52K dump; restore
  into a scratch database with `scripts/restore-roles.sql` exited 0;
  `SET ROLE app_runtime; SELECT count(*) FROM clients` returned 2;
  `SET app.tenant_id='restore-check'; SELECT count(*) FROM
  project_centrum.sports_facilities` returned 1.
- YAML parse of `backup.yml`, `schema-check.yml`, and `ci.yml` — pass.
- `bash scripts/lint-tests.sh` — pass.
- `bash scripts/lint-migration.sh` — `migrations additive-clean (95 files checked)`.
- Final full local suite: `go test -p 1 ./... -count=1` — `exit=0`.
- Production backup run 45078864220: dump + restore-smoke green.
- Production `db-migrate` run 45079048441: applied `082` and `084`; ledger
  check clean. Follow-up run 45084687570 applied `085` and `086` after the
  new binary was live. `schema-check` run 45084851243 and `smoke` run
  45084851034 passed.
- Production ADK smoke after `085`: `POST /v1/agent/client-chat` returned a
  reply (the session service set `app.tenant_id` and the scoped policies
  allowed the write/read).
- Architecture-closure production rollout: the new gate exposed an empty
  `centrum` entitlement. Production and staging were backfilled to the
  project manifest set; `088_project_entitlement_backfill.sql` records the
  one-time normalization, and `migrate check` now rejects an empty project
  entitlement.
- Architecture-closure local suite: `go test -p 1 ./... -count=1` — pass;
  `go vet ./...` clean. Targeted API tests cover the platform vertical pin,
  project-vertical immutability, platform feature revalidation, canonical
  feature writes, public feature entitlement, and the aggregate-only
  `/healthz` body.
- Exit-gate local verification (2026-11-24):
  - `bash scripts/test-local.sh -count=1` was run twice consecutively. Both
    runs created a fresh `agents_proxy_test_*` database, applied the embedded
    chain, ran the full suite, exited 0, dropped the database, and left no
    `agents_proxy_test_*` database or lock behind.
  - `go test -p 1 ./internal/features ./internal/api ./cmd/migrate ./cmd/smoke ./cmd/testdb ./cmd/bench ./cmd/server`
    — pass. New coverage includes `TestValidateClientEntitlement`,
    `TestAuditProjectEntitlementsFencesEmptyProject`,
    `TestCentrumStaffSession_FullRouter`,
    `TestAdmin_TenantHealthAndReload` (empty entitlement → 422, fixed → 200),
    `TestReconcileQuarantineReplacesStaleBootFences`, and the `cmd/smoke`
    contract table tests.
  - `bash scripts/bench.sh` local full workload (concurrency 16, 5 s):
    health p95 4.83 ms, facilities 20.86 ms, availability 18.37 ms, pricing
    15.65 ms, records list 37.20 ms, records create/delete 137.59 ms, staff
    login 385.54 ms, admin clients 99.52 ms, admin health 22.70 ms; 0 errors.
    Agent mock workload: `agent-client-chat` p95 12.56 ms, `chat` p95
    15.03 ms; 0 errors.
  - Production read-only baseline at 0.2 RPS: health p95 138.43 ms,
    facilities 145.38 ms, availability 137.90 ms, pricing 168.43 ms; all 200
    and 0 unexpected 5xx. Higher probe rates produced expected 429s from the
    per-IP limiter and were excluded from latency results.
  - Local cold-start proxy (5 process starts): 31,705 / 1,751 / 1,301 / 6,061
    / 782 ms. The first run is OS file-cache dominated; Vercel function cold
    start remains unmeasured and is called out in the baseline report.
  - YAML parse of `deploy.yml`, `smoke.yml`, `ci.yml`, and `schema-check.yml`
    — pass; `bash -n scripts/test-local.sh scripts/bench.sh` — pass.
- Production gate rollout and verification (2026-11-24):
  - `schema-check` run 45362695377 — `migrations clean`.
  - CI/security PRs #87, #88, #89 merged. The first gate runs proved
    `migrate check`, build, staging, the Deployment Protection bypass, and
    `cmd/smoke` on the staged URL. They stopped at promote because Vercel CLI
    66.6.0 rejects scoped tokens during its `/v2/user` preflight
    (`User not found`).
  - The gate now uses the project-scoped Vercel REST API for promote,
    rollback, and staged-deployment cleanup. Deploy run 45439936391 passed
    `migrate check` → stage → smoke → API promote → production smoke.
  - Production is on `dbf8225`; `/healthz` reports
    `status=ok db=true redis=true` and the matching version. `cmd/smoke`
    passes health, Centrum facilities/availability/pricing, records 401,
    legacy-alias 404, and CORS preflight.
  - Independent `smoke` workflow run 45440953568 was re-run successfully
    after a manual backup (run 45441755905); the earlier failure was the
    backup-freshness check, not the deploy contract.
  - Strict-feature rollout guard is live: Centrum's `project.json` now sets
    `strict_features: true`; deploy run 45464744016 passed `migrate check`
    against production with the full 14-feature set.
  - Controlled rollback drill run 45470318895 passed:
    `dpl_DwRFXxjmHAGSLooz8TZqiUL4X8TJ` (8f358d7) → previous
    `dpl_BQbi7TvdN9nvooXTLs7zSd7x9q6g` (3c67104) and back, with the expected
    `/healthz` version asserted at each step.
  - Free-plan guards landed: backup has two cron offsets and smoke triggers a
    replacement backup when stale; failed smoke/perf probes open a single
    GitHub issue; daily `perf-guard` was calibrated and then passed (run 45477061415): US-runner
    p95 was health 271 ms, facilities 325 ms, availability 316 ms, pricing
    282 ms, all 0 errors. Those are regression ceilings, not the sin1
    same-region SLOs; the false `[perf-alert]` issue #106 was closed with that
    explanation.
  - CI supply chain: Actions are SHA-pinned, `govulncheck` is pinned, and
    Dependabot opened its first Go/Actions update PRs. Vercel CLI remains
    pinned to 66.6.0. Backup with the pinned actions/upload/download
    artifacts passed in run 45479141585.
- Production/staging rollout of A-10 and A-13 is complete for production.
  The production token remains project-scoped; team/full-account scope is not
  required because promotion uses the REST API.
- Production health on `f52cc76`: `status=ok`, `db=true`, `redis=true`;
  `/v1/sports/facilities` 404, `/v1/p/centrum/facilities` 200; Centrum and
  DFD frontends healthy.
- Staging after `085`/`086`: `migrations clean`; preview `1e531cc` healthy
  with the same 404/200 route contract; the four ADK policies and the usage
  insert policy match the production definitions.

## Accepted residual / follow-up

- **Docker build** is fixed in `.dockerignore` and added to CI, but Docker is
  not installed on this workstation; the CI job is the verification gate.
- `ops/backup.sh` also ran successfully as the real `backup_reader` role
  (local test credential), proving the BYPASSRLS dump path still produces
  rows after removing `--no-owner --no-privileges`.
- Vercel Production was checked (names only): `BACKGROUND_TICKERS`,
  `DB_USE_SIMPLE_PROTOCOL`, `DB_POOL_MIN`, `DB_POOL_MAX`, and
  `EXPECTED_DB_REF` are present and match the new server-side validation.
- The hard deploy gate is live on `main`; production is on `8f511ef` after a
  successful gate run. The `production` environment has `VERCEL_TOKEN`,
  `PROD_DATABASE_ADMIN_URL`, `VERCEL_AUTOMATION_BYPASS_SECRET`,
  `VERCEL_ORG_ID`, `VERCEL_PROJECT_ID`, and `PROD_DB_REF`.
- A manual Vercel dashboard redeploy bypasses `deploy.yml`; the runbook marks
  it break-glass and requires `cmd/migrate check` first.
- The API-based staging-deployment DELETE is implemented but not exercised in
  a live failure path. Capture now matches `/healthz` by version, so a stale
  unaliased deployment cannot be mistaken for the current alias; cleanup
  failure only leaves an unused deployment.
- GitHub Actions red is still not a pager. Failed smoke/perf jobs now open a
  persistent issue, but an external on-call integration is still recommended.
- PITR is intentionally disabled on the free plan. Backup RPO remains
  best-effort, now with two cron offsets and a stale-backup auto-trigger.
- The performance baseline is local + read-only production probes. The
  post-gate 48-sample spot check had stable medians but multi-second tail
  outliers; absolute production p95 is unverified until server time and cold
  instances are separated. See `docs/PERF-BASELINE-2026-11.md`.

## Implementation map

| Item | Main files |
|---|---|
| P0-1 | `internal/features/gate.go`, `builder.go`, `project_builder.go`, `internal/api/router.go`, `internal/authapi/provider.go`, `internal/api/platform_routes_isolation_test.go` |
| P0-2 | `internal/authapi/authapi.go`, `session.go`, `provider.go`, `internal/authapi/authapi_test.go`; no cached authorization state |
| P0-3 | `internal/db/migrations/082_revoke_agency_runtime_grants.sql`, `internal/db/contract_test.go` |
| P0-4 | `ops/backup.sh`, `.github/workflows/backup.yml`, `scripts/restore-roles.sql` |
| P0-5 | `scripts/test-local.sh`, `internal/testsafety/guard.go`, `internal/db/guard_test.go`, `internal/api/api_test.go`, `internal/dbtest/clone_test.go`, `projects/centrum/backend/testutil/testutil.go` |
| P0-6 | `.github/workflows/backup.yml`, `schema-check.yml` |
| P1-1 | `internal/tenantdb/tenantdb.go`, `internal/features/shared/records/{handlers,feature_adapter,identity}.go`, `internal/features/project_builder.go` |
| P1-2 | `internal/db/migrations/{084_drop_usage_read_policy,085_adk_sessions_tenant_rls,086_usage_client_scope}.sql`, `internal/adkrunner/pgx_sessions.go`, `internal/usage/store.go`, `internal/db/{adk_rls_test,rls_enable_test,definers_test}.go`, `ARCHITECTURE.md` |
| P1-3 | `internal/agentconfig/{config,load}.go`, `internal/features/registry.go`, `internal/db/project_migrations.go`, `cmd/migrate/main.go` |
| P1-4 | `internal/api/agent_chat.go` |
| P1-5 | `internal/db/{migrate,migration_status,project_migrations}.go`, `ops/backup.sh` |
| P1-6 | `.dockerignore`, `.github/workflows/ci.yml` |
| P1-7 | `internal/config/{config,database,server}.go`, `cmd/server/main.go`, `.env.staging.example` |
| P1-8 | `cmd/provision/main.go`, `cmd/migrate/main.go`, `.github/workflows/db-migrate.yml` |
| S-1 | `internal/authapi/authapi.go`, `internal/config/config.go`, `cmd/server/main.go`, `internal/authapi/authapi_test.go`, `.env.example` |
| S-2 | `internal/pushsafe/*`, `internal/api/push.go`, `internal/reminder/push_sender.go` |
| S-3 | `internal/db/migrations/087_admin_audit_log.sql`, `internal/api/admin_audit.go`, `internal/auth/admin.go`, `internal/api/router.go`, `internal/db/contract_test.go` |
| S-4 | `internal/middleware/{logging,recovery}.go`, `internal/api/report_jobs.go` |
| A-1 | `internal/features/{builder,gate}.go`, `internal/api/platform_routes_isolation_test.go` |
| A-2 | `internal/features/{gate.go,clientctx/clientctx.go}`, `internal/features/shared/records/{identity,tenant,client_ctx}.go`, `internal/authapi/provider.go`, `projects/centrum/backend/auth/provider.go` |
| A-3 | `internal/api/admin.go`, `internal/api/admin_patch_test.go` |
| A-4 | `internal/requestip/*`, `internal/middleware/ratelimit.go`, `internal/agency/ratelimit/*`, `internal/agency/admin/handler.go` |
| A-5 | `internal/config/{config,secure_cookies_test}.go`, `internal/api/router.go` |
| A-6 | `cmd/server/main.go`, `internal/api/{router,admin_projects}.go`, `internal/features/validate.go`, `AgentsTools/projects.go`, `internal/db/project_migrations.go`, `cmd/migrate/main.go` |
| A-7 | `internal/projecthooks/*`, `projects/centrum/backend/bookings/feature.go`, `projects/centrum/agent/tools/book_slot.go`, `cmd/server/main.go` |
| A-8 | `internal/adkrunner/runner.go` |
| A-9 | `internal/api/healthz.go`, `internal/api/api_test.go` |
| A-10 | `.github/workflows/deploy.yml`, `vercel.json`, `cmd/smoke/*`, `.github/workflows/smoke.yml`, `RUNBOOK.md`, `ops/RUNBOOK-vercel.md` |
| A-11 | `internal/features/entitlement.go`, `cmd/server/main.go`, `internal/api/tenant_health.go`, `cmd/migrate/main.go` |
| A-12 | `cmd/testdb/*`, `scripts/test-local.sh`, `.env.test.example`, `.github/workflows/ci.yml` |
| A-13 | `cmd/bench/*`, `cmd/benchseed/*`, `scripts/bench.sh`, `docs/PERF-BASELINE-2026-11.md` |
| A-14 | `internal/agentconfig/{config,load}.go`, `internal/features/entitlement.go`, `projects/centrum/project.json` |
| A-15 | `.github/workflows/{backup,smoke,perf-guard}.yml` |
| A-16 | `.github/workflows/*.yml`, `.github/dependabot.yml` |
- P1-2 regression: `TestADKSessionsTenantRLS` (unset GUC sees 0, matched
  GUC sees only its app, cross-tenant insert rejected) and
  `TestRecord_UsesClientGUC` (usage insert as `app_runtime` works only with
  `app.client_id`) pass.
- Final P1-2 full suite: `go test -p 1 ./... -count=1` — `exit=0`.
- Security polish targeted tests:
  `go test -p 1 ./internal/pushsafe ./internal/middleware` — pass;
  `go test ./internal/authapi -run 'TestLogin|TestUserByToken'` — pass
  (includes `TestLoginLockoutAndRecovery`, `TestLoginRateLimitedPerAccount`);
  `go test ./internal/api -run 'TestAdminAuditWritesRow|TestValidPushKeys'` — pass;
  `go test ./internal/reminder -run TestPushSender` — pass;
  `go test ./internal/db -run TestAgencyTablesAreNotRuntimeAccessible` — pass.
- Security-polish full suite: `go test -p 1 ./... -count=1` — `exit=0`.
- Security-polish rollout: PR #80 (`ci` run 45089473953; `gitleaks` run
  45089473965) is merged. Production `db-migrate` run 45088915444 applied
  `087_admin_audit_log.sql`; staging applied the same migration through
  `scripts/migrate-staging.sh`. Both ledgers contain `087`; production and
  staging have `public.admin_audit_log` present with RLS enabled, zero
  policies, and no `app_runtime` table or sequence privileges. The live admin
  key is a Vercel Sensitive value and cannot be read by the CLI, so a manual
  admin request was not automated; the middleware path is covered by
  `TestAdminAuditWritesRow` and the production/staging grant checks.
- Post-deploy checks: production `01e2c9c` and staging `b703acc` report
  `{"status":"ok","db":true,"redis":true}`; `/v1/sports/facilities` is 404
  and `/v1/p/centrum/facilities` is 200. `schema-check` run 45090823801,
  `smoke` run 45090823917, and backup/restore run 45090982743 passed.
- Backup cadence/monitor: PR #82 tightened the logical dump to every 2 hours
  and added a `smoke` backup-freshness check (4-hour threshold); PR #83
  fixed the check's repository discovery. Post-change `smoke` run 45163919556
  passed with the freshness job, and backup run 45164000513 passed the dump
  and restore smoke. PITR remains the only guaranteed RPO option and is not
  enabled.
