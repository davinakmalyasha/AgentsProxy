# Governance — branches, DDL, testing (slim)

## Branches

`main` = prod. Short-lived `feat/*`, `fix/*`, `chore/*`, and `docs/*`
branches are cut from `main` and merged back through a PR with green CI.
Tags `v*` are cut from `main` only. The long-lived `staging` branch exists
only to receive Preview deployments with branch-scoped staging variables
(see `ops/RUNBOOK-staging.md`); ordinary PR previews stay database-less.

Secrets in deploy-target env vars only. `.env` is local, gitignored.

## Environments

`ENV` is a strict enum: `development`, `staging`, `production`. Anything
else fails boot. The two safety invariants are enforced in
`internal/config` before a database connection is opened:

| | Database | `ENV` | `RUN_MIGRATIONS` |
|---|---|---|---|
| Local | `localhost` only | `development` | `true` (default) |
| Staging | remote, explicit `ALLOW_REMOTE_DB=true` | `staging` | `false` |
| Production | remote | `production` | `false`; DDL only via `db-migrate.yml` |
| Vercel Preview | none until staging exists | never `production` | always `false` |

* A non-production process refuses a non-loopback database host. Staging is
  the only exception, and only with the explicit flag.
* `EXPECTED_DB_REF` pins staging and production to their own Supabase
  project. It is optional for local development; when set, a copied DSN
  fails boot before the pool opens.
* Production boots with `DATABASE_URL` as `app_runtime`; a superuser or
  `BYPASSRLS` role fails the boot. The admin pool stays separate.
* Project schemas must match `project_*`; the database check constraint
  enforces the namespace, and the partial unique index
  `clients_project_schema_key` allows exactly one client per project schema.
  Admin provisioning plus `migrate check` enforce the same invariant.
  `public` is only for the legacy platform tenant row (dfd-agency).
* Integration tests must give every tenant its own cloned schema
  (`internal/dbtest` / `testutil.CloneTenantSchema`). Feature tests may not
  hardcode `project_centrum`; `scripts/lint-tests.sh` fails CI on a regression.
* Vercel Preview must not carry production secrets. A preview backend is
  expected to fail closed rather than touch production.
* Production and staging run PostgreSQL 17; CI and compose use
  `postgres:17-alpine`.
* Tests read `TEST_DATABASE_URL`, `TEST_DATABASE_ADMIN_URL`, and
  `TEST_REDIS_URL` only. `make test` loads `.env.test` and refuses a remote
  host.

## DDL law (expand/contract)

Add: `ADD COLUMN NULL/default`, `CREATE INDEX`, new `CHECK NOT VALID` +
`VALIDATE`. Destructive changes (`DROP`, `ALTER TYPE`, `RENAME`) are a later
contract migration after the stability window, never in the same release that
deploys the code depending on them. Never edit an applied migration — add a
new one. Naming: `sports_*` for vertical tables, agency tables keep their DFD
names (migration 068). CI lints non-additive DDL; the fresh-DB job proves the
chain runs clean, and the contract tests pin the final state.

## Testing

* Unit (no infra, parallel): pricing, validators, rate-limit math.
* Integration (ephemeral PG + Redis, `go test -p 1`): every feature ships
  `feature_test.go` with isolation (A creates, B gets 404) + its invariant
  (e.g. double-booking → exactly one 201 + one 409). `internal/db` also
  proves the platform invariant with two project schemas
  (`TestTwoProjectSchemasAreIsolated`).
* **Never run DB-backed tests against a production DSN.** Tests read
  `TEST_DATABASE_*` only; `DATABASE_URL` is for the server. Point
  `TEST_DATABASE_ADMIN_URL` at a disposable local database. Leftover demo
  tenants can be removed with `scripts/prune-demo-tenants.sql` (see
  RUNBOOK §8a). Destructive migration tests additionally require
  `MIGRATE_TEST_DESTRUCTIVE=true` and a loopback host.
* Production smoke: `/healthz`, Centrum availability/pricing, DFD catalog
  reads, and owner login. Tenant-specific E2E runs on a disposable database
  or a scoped preview environment.

`SELECT` explicit columns, never `*`. Tolerate two schema versions during
rollout (read-coalesce). Tenant tables: `WithTenantTx` GUC + RLS always.
