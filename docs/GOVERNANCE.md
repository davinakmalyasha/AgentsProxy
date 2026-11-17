# Governance — branches, DDL, testing (slim)

## Branches

`main` = prod (PR + green + reviewer, tags `v*` cut here only).
`staging` = pre-prod mirror (auto-deploys). `feat/*,fix/*` cut from `staging`,
merge back via PR. `staging → main` is the release PR (migration diff required
reading). No direct pushes to `main`/`staging`.

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
* E2E on staging: owner login → book → pay → export → notify, `/healthz`,
  two-tenant RLS matrix.

`SELECT` explicit columns, never `*`. Tolerate two schema versions during
rollout (read-coalesce). Tenant tables: `WithTenantTx` GUC + RLS always.
