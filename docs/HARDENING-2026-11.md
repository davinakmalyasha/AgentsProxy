# Architecture hardening — November 2026

Status: **P0/P1 and security-polish controls implemented and verified; residual ops items are marked**. This
document is the single tracker for the four-audit review (routing/layering, database/RLS, auth/isolation,
docs/ops/CI). An item is only marked `[x]` when the code is merged and
the named verification passes.

Legend: `[x]` implemented and verified · `[~]` implemented, verification
pending · `[ ]` open · `[-]` accepted residual, documented.

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
  `smoke` run 45090823917, and backup/restore run 45090982743 passed. The
  backup schedule was tightened to every 2 hours and `smoke` now checks
  backup freshness (4-hour threshold) in PR #82; run 45090982743 proves the
  dump/restore path used by the schedule.
