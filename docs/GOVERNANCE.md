# Governance — branching, CI, environments, testing, tenants

Single home for development rules. Read this before opening a PR.
Issue log lives in `docs/refinementNotes.md`; architecture in `ARCHITECTURE.md`;
operations in `RUNBOOK.md`.

Applies to both repos: `AgentsProxy` (backend) and `CentrumBadminton` (frontend).

---

## 1. Branches

| Branch | Role | Protection |
|---|---|---|
| `main` | Production. Deploys to prod. | No direct pushes. PR + all checks green + 1 reviewer. Tags cut from here only (`v*.*.*`). |
| `staging` | Pre-production mirror. Auto-deploys to staging env. | No direct pushes. PR + all checks green. |
| `feat/*`, `fix/*` | Work branches, cut from `staging`, merged back into `staging` via PR. | Checks must pass. |

Promotion `staging → main` is its own PR (the release PR) — this is the human
go/no-go. The release PR must include the migration diff as required reading.

Required repo hygiene (both repos): `CODEOWNERS`, PR template, Dependabot,
required-status-checks in GitHub settings. No exceptions.

## 2. Environments

| | Dev (local) | Staging | Prod |
|---|---|---|---|
| Backend DB | docker-compose Postgres | Second Supabase project | Current Supabase project |
| Backend deploy | `docker compose up` | Staging project, auto-deploys `staging` | Prod project, deploys `main` only |
| Frontend | `next dev` | Preview project → staging proxy | Prod project |
| Migrations | wipe-and-reload freely | Run here FIRST, soak, then prod | Only after staging proves them |
| Seed | `seed-dev.mjs` | Permanent realistic demo tenant | Real data only |

**Iron law: prod boot is never the first place a migration runs.**
Staging runs it first. Staging doubles as the permanent demo environment.

**Secrets rule:** prod secrets live in deploy-target env vars (Vercel dashboard,
Railway env, server env files created on the machine) — never in repo files.
`.env` is local-only, gitignored, and must never leave the machine. Prod
credentials in a workstation file are physical-world risk (theft, shared
backups, borrowed laptop), not just repo risk.

## 3. DDL law (additive only)

- Allowed in a migration: `ADD COLUMN … NULL` (or with DEFAULT), `CREATE INDEX`
  (concurrently, outside the boot tx), new `CHECK … NOT VALID` then `VALIDATE`.
- Forbidden without a phased plan: `DROP COLUMN`, `ALTER TYPE`, `RENAME`.
- Never edit an applied migration — add a new one that does the delta.
- Table naming: prefix per vertical (`sports_*`, `salon_*`, …). Nothing enforces
  this except review — review enforces it.
- CI lints this: a migration-lint job fails the PR on non-additive DDL, and a
  fresh-DB job proves the full chain runs clean on an empty database.

Rationale: one bad migration aborts the single boot-time chain for every
tenant. Additive DDL cannot break existing queries, so it cannot break
existing tenants.

## 4. CI gates — TARGET (not-yet-enforced; do not treat as passing today)

Backend PR checks: `gofmt` + `go vet` + `golangci-lint` + `go build` +
`go test -race` + coverage with enforced threshold (start 60%, ratchet up) +
migration-lint + fresh-DB migrate + `govulncheck` + `gitleaks` + CodeQL.

Frontend PR checks: `eslint` + `tsc --noEmit` + `vitest run` + `next build`.

Contract check: backend publishes a versioned route inventory; CI fails on
breaking changes unless the release PR declares them, and the frontend
connector tests run against the new contract.

Nightly + pre-promotion on staging: end-to-end
(owner login → book → pay → export → notify), `/healthz`, and the two-tenant
RLS matrix (tenant B lists empty, B reads A's row → 404).

## 5. Testing strategy (three layers)

- **Unit** — no infra, parallel: pure logic (pricing math, cart totals,
  validators, sanitizers, rate-limit).
- **Integration** — ephemeral Postgres + Redis, serial (`-p 1`) within the job:
  every feature ships `feature_test.go` with isolation + invariants
  (tenant A creates, tenant B gets 404). Role-tier tests are the security
  tripwire — any tier change must update them first.
- **E2E on staging** — real deploy, real DB: the nightly + promotion suite.

`SELECT` explicit column lists, never `SELECT *`. Handlers tolerate two
schema versions during rollout (read coalesce, write new only when the
tenant's contract allows).

## 6. Tenant isolation (never regress these)

- Every tenant table: `tenant_id REFERENCES clients(tenant_id) ON DELETE
  CASCADE` + `ENABLE + FORCE RLS` + policy
  `tenant_id = current_setting('app.tenant_id', true)` + `GRANT … TO app_runtime`.
- Every handler: `records.WithTenantTx` (sets the GUC per transaction).
  The database enforces the boundary, not just the code.
- Anon tier (`POST /v1/agent/client-chat`): pricing + availability tools only.
  No tool that returns tenant rows (bookings, customers, staff) is ever added
  to anon. Public contact is one owner-curated string, never the staff table.
- `OwnerTenant`-style per-tenant code branches do not exist and must not be
  introduced: same vertical ⇒ same code, rows separate tenants.
- Prompt/skill/information files live in `tenants/<slug>/` (code, reviewed,
  versioned). No prompt content in database rows.

## 7. Tenant onboarding runbook (adding tenant #2 without touching #1)

1. Work branch from `staging`: copy `tenants/_template/` →
   `tenants/<slug>/`; set `memory/project.json` (`slug` must equal the future
   `clients.tenant_id`); write prompt/information/skills; add tools
   (`agentstools.Register("<slug>", …)`, `Mutating: true` on writes);
   add namespaced migration (`NNN_<vertical>_*.sql`) only if new tables needed.
2. Blank-import new packages in `cmd/server/main.go`. New feature ships its
   `feature_test.go` (isolation + invariants) in the same PR.
3. `POST /v1/admin/clients {name, tenant_id, vertical}` provisions the row
   (token shown once).
4. PR into `staging`: all checks green + reviewer. Staging auto-deploys;
   migration runs against staging DB first.
5. Re-run e2e + RLS matrix including the new tenant. Soak.
6. Release PR `staging → main`. Tag. Prod deploys.
7. Verify on prod via the per-tenant health endpoint before the tenant takes
   traffic. If quarantined, fix forward — other tenants are unaffected.

If any step fails, the failure is contained to the new tenant or to staging.
Prod tenants never see it. That property is the point of this document.
