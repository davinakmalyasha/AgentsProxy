# Governance — branches, DDL, testing (slim)

## Branches

`main` = prod. Short-lived `feat/*`, `fix/*`, `chore/*`, and `docs/*`
branches are cut from `main` and merged back through a PR with green CI.
Tags `v*` are cut from `main` only. There is no long-lived `staging`
branch in this repository; preview deployments cover pre-prod checks.

Secrets in deploy-target env vars only. `.env` is local, gitignored.

## DDL law (additive only)

Add: `ADD COLUMN NULL/default`, `CREATE INDEX`, new `CHECK NOT VALID` +
`VALIDATE`. Never: `DROP`, `ALTER TYPE`, `RENAME` without a phased plan.
Never edit an applied migration — add a new one. Naming: `sports_*` for
vertical tables, agency tables keep their DFD names (migration 068).
CI lints non-additive DDL; fresh-DB job proves the chain runs clean.

## Testing

* Unit (no infra, parallel): pricing, validators, rate-limit math.
* Integration (ephemeral PG + Redis, `go test -p 1`): every feature ships
  `feature_test.go` with isolation (A creates, B gets 404) + its invariant
  (e.g. double-booking → exactly one 201 + one 409).
* **Never run DB-backed tests against the production `DATABASE_URL`.**
  Point local/CI tests at a disposable database. Leftover demo tenants can
  be removed with `scripts/prune-demo-tenants.sql` (see RUNBOOK §8a).
* Production smoke: `/healthz`, Centrum availability/pricing, DFD catalog
  reads, and owner login. Tenant-specific E2E runs on a disposable database
  or a scoped preview environment.

`SELECT` explicit columns, never `*`. Tolerate two schema versions during
rollout (read-coalesce). Tenant tables: `WithTenantTx` GUC + RLS always.
