# Staging on the free plan

Staging uses the native free-tier features, not custom orchestration:

- **Supabase:** a second Free project (the plan includes two active projects).
  Branching is Pro-only and, more importantly, builds a branch from
  `supabase/migrations` or the Supabase migration history. This repo's
  schema is built by `go run ./cmd/migrate`, so a Supabase branch would come
  up without the platform/project tables unless we mirrored every migration.
  A second project is simpler and works with the existing runner.
- **Vercel:** a long-lived `staging` Git branch in the existing
  `agents-proxy` project. Vercel Custom Environments are Pro-only; on Hobby
  the supported staging workflow is a Preview branch with branch-scoped
  environment variables. Preview deployments already have `VERCEL_ENV=preview`,
  which is the only target that accepts `ENV=staging`.

Do **not** point the shared Preview environment at staging. Keep ordinary
PR previews database-less and fail-closed; only the `staging` branch gets
staging credentials.

## 1. Create the staging Supabase project

Dashboard → New project:

- Name: `agents-proxy-staging`
- Region: `ap-southeast-1` (same as production)
- Plan: Free
- Save the database password in the password manager.

Collect the exact Connect strings from the staging project. Do not reuse
the production host and change only the ref: staging may be on
`aws-1-ap-southeast-1.pooler.supabase.com` while production is on
`aws-0-...`. The pooler will answer `tenant/user ... not found` if the host
and project ref do not belong together.

- Runtime pooler DSN (port **6543**, user `app_runtime.<ref>`).
- Admin DSN (port **5432**, user `postgres.<ref>`).
- A separate Redis instance (Upstash free or a dedicated database) —
  staging must not share production Redis keys. Copy the **URL**, not the
  `redis-cli ...` command. Upstash requires TLS, so use `rediss://`, not
  `redis://`.
- An LLM key/budget for staging. It may be the same provider key, but keep
  the spend cap separate.

## 2. Apply the embedded migration chain

The migration chain creates `app_runtime`, `auth_definer`, `money_reader`,
`backup_reader`, and `delivery_caller`. Until it runs, `ALTER ROLE
app_runtime` fails with `role "app_runtime" does not exist`.

Copy `.env.staging.example` to `.env.staging` (gitignored), fill the staging
admin DSN, then:

```bash
bash scripts/migrate-staging.sh
```

The script refuses the production project ref and any non-Supabase host.
After it succeeds, set the runtime password:

```sql
ALTER ROLE app_runtime PASSWORD '<random-staging-password>';
```

Use a generated value (`openssl rand -hex 24` or a password manager). This
is the password for the **app_runtime** role, not the `postgres` admin
password. The same pattern applies to `backup_reader` and `delivery_caller`
if you test backup/delivery on staging.

This creates the platform tables, `app_runtime`, `auth_definer`,
`money_reader`, `backup_reader`, `project_centrum`, and applies Centrum's
project migrations (`0001` schema move, `0002` records, `0003` FORCE RLS).
Verify:

```sql
select count(*) from pg_namespace where nspname = 'project_centrum';
select tenant_id, db_schema from public.clients;
select version from public.project_migrations;
```

The staging project starts empty. Seed a synthetic tenant with
`scripts/seed-dev.mjs` (or SQL); never restore production data into staging.

## 3. Add the staging branch

```bash
git switch -c staging
git push -u origin staging
```

The branch exists only to receive Preview deployments with staging
variables. Merge `main` into it before testing a release.

## 4. Add branch-scoped Preview variables

Use the dashboard (Environment Variables → Preview → branch `staging`) or
the CLI. Do **not** remove a shared variable with
`vercel env rm NAME preview`; the CLI can remove the Production target too.
Add separate entries instead.

| Variable | Value |
|---|---|
| `ENV` | `staging` |
| `ALLOW_REMOTE_DB` | `true` |
| `DATABASE_URL` | staging `app_runtime` pooler, port `6543`, `?pgbouncer=true` |
| `DATABASE_ADMIN_URL` | staging `postgres` direct, port `5432` |
| `EXPECTED_DB_REF` | staging Supabase project ref (blocks a copied production DSN) |
| `DB_USE_SIMPLE_PROTOCOL` | `true` |
| `DB_POOL_MIN` / `DB_POOL_MAX` | `0` / `2` |
| `REDIS_URL` | staging Redis only |
| `LLM_BASE_URL` / `LLM_MODEL` / `LLM_API_KEY` | staging budget |
| `ADMIN_API_KEY` | staging-only random value |
| `JWT_SECRET` | optional staging-only 32+ char value; enables agency auth/portal routes |
| `SIGNUP_ENABLED` | `false` unless testing self-service signup |
| `RUN_MIGRATIONS` | `false` |
| `BACKGROUND_TICKERS` | `false` |
| `ALLOW_ORIGINS` | staging frontend origin |
| `SENTRY_ENV` | `staging` |

`VERCEL_ENV=preview` + `ENV=staging` is accepted by
`internal/config.validateDeployTarget`. A separate Vercel project deployed
to its Production target would see `VERCEL_ENV=production` and correctly
refuse `ENV=staging`, which is why the branch approach is used.

### Staging scope

The client plane is a full rehearsal: separate database, Redis, migrations,
Centrum project routes, and agent calls. The agency plane needs one more
step: set a staging-only `JWT_SECRET` (32+ chars) and seed a staging
`users` row before testing `/api/v1/auth/*`, the portal, or admin screens.
Without `RESEND_API_KEY`, OTP/signup codes are logged to the staging
function logs instead of emailed; that is acceptable for staging, not for
production. Google login and Supabase Storage stay disabled unless their
staging-scoped values are added.

Use a separate LLM key or a hard provider-side budget for staging. The
runbook permits sharing the provider key, but nothing in the app enforces a
per-environment spend cap.

## 5. Verify

```bash
curl -s https://<staging-preview-url>/healthz
curl -s "https://<staging-preview-url>/v1/p/centrum/facilities"
```

Logs must show:

```text
migrations skipped (RUN_MIGRATIONS=false; run as a deploy step)
runtime role verified
origins loaded
```

`runtime role verified` only runs when `ENV=production`; on staging it is
expected to be absent. The database check is `/healthz` and a tenant read.

## 6. Rehearse releases

1. Merge `main` into `staging`.
2. Apply pending migrations to the staging admin DSN.
3. Let the `staging` branch redeploy.
4. Run the smoke reads and one agent chat.
5. Merge `staging` into `main` only after the rehearsal passes.

The normal PR Preview environment remains DB-less. That is deliberate: a
shared staging database across every PR would let one preview mutate
another's data.
