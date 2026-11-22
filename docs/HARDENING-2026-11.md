# Architecture hardening — November 2026

Status: **in progress**. This document is the single tracker for the
four-audit review (routing/layering, database/RLS, auth/isolation,
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

- [~] **P1-2 RLS regressions and honesty.**
  `063` re-adds the blanket `usage_read_runtime` policy that `053`
  deliberately dropped; `usage` INSERT is `WITH CHECK (true)`; ADK
  session tables use `USING (true)` with no tenant predicate. Fix: drop
  the usage read policy, scope the write, and tenant-scope the ADK
  tables or document the application-enforced boundary explicitly.
  Document RLS as defense-in-depth, not the sole boundary.

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
  serialize the whole `MigrateAll`. Reconcile the `backup_reader`
  attributes.

- [~] **P1-6 Docker/release build.**
  `.dockerignore` excludes `*.md`, but `projectembed.go` embeds
  `projects/*/agent/agent.md` and skills. `make docker` and
  `release.yml` cannot build. Fix `.dockerignore` and add a Docker build
  to CI.

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
- [x] **P2-3** Backup comments say 6-hourly, not nightly/unscheduled.
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

## Remaining ops backlog (separate from architecture)

- [x] Centrum `next`/`eslint-config-next` → 16.4.6; Dependabot #16 closed.
  `npm audit --omit=dev` is clean, `npm run check` is 14 files/159 tests, and
  `npm run build` passes.
- [ ] Backup RPO: PITR if budget allows, else a reliable 2-hour scheduler.
  Restore correctness is fixed; the RPO decision still belongs to the
  operator.
- [~] DFD: `api.dfdagency.com` plus one shared origin config. The two
  hardcoded copies now import `client/api-origin.mjs`; the production pin and
  preview fail-closed behavior are unchanged. Adding the custom domain is a
  DNS decision.
- [ ] GitHub Team + second reviewer, or documented accepted risk.

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

## Accepted residual / follow-up

- **ADK session tables** (`sessions/events/app_states/user_states`) still use
  `USING (true)`; `pgx_sessions.go` filters `app_name` in every query. This is
  application-enforced. A tenant_id column + restrictive policy is a
  security-pass migration, not an architectural prerequisite for the route
  and auth fixes above. Documented in `ARCHITECTURE.md`.
- **Docker build** is fixed in `.dockerignore` and added to CI, but Docker is
  not installed on this workstation; the CI job is the verification gate.
- **Login rate limit / lockout, push SSRF allowlist, admin audit trail,
  report-token log redaction** remain in the security-polish phase; they are
  additive controls, not broken invariants.
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
| P1-2 | `internal/db/migrations/084_drop_usage_read_policy.sql`, `internal/db/rls_enable_test.go`, `ARCHITECTURE.md` |
| P1-3 | `internal/agentconfig/{config,load}.go`, `internal/features/registry.go`, `internal/db/project_migrations.go`, `cmd/migrate/main.go` |
| P1-4 | `internal/api/agent_chat.go` |
| P1-5 | `internal/db/{migrate,migration_status,project_migrations}.go`, `internal/db/migrations/083_backup_reader_reconcile.sql` |
| P1-6 | `.dockerignore`, `.github/workflows/ci.yml` |
| P1-7 | `internal/config/{config,database,server}.go`, `cmd/server/main.go`, `.env.staging.example` |
| P1-8 | `cmd/provision/main.go`, `cmd/migrate/main.go`, `.github/workflows/db-migrate.yml` |
