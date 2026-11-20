# Vercel deployment (Go service)

Separate Vercel **project** from every client frontend. Two reasons, both
load-bearing rather than cosmetic:

1. **Secrets.** This service holds `LLM_API_KEY`, `ADMIN_API_KEY`,
   `DATABASE_URL`, `REDIS_URL` — platform-wide
   secrets shared by every tenant. A frontend project must never be able
   to read them. Separate projects give the Go service its own env
   namespace.
2. **Deploy independence.** A Go change should not rebuild and redeploy
   every client site, and one client's build failure must not block the
   backend.

The Go runtime auto-detects `cmd/server/main.go` and sets `GOOS=linux`,
producing a native binary — **not** Wasm. Vercel downloads the toolchain
named in the `go` directive and caches it; the first build on a brand-new
Go version can fail while the mirror catches up.

Each client frontend is its own project. Vercel Hobby quotas are
**per account**, not per project, so splitting costs no extra quota.

## Env vars

Set in the dashboard, never in git.

> **Never run `vercel env rm NAME preview` for a variable that is shared
> with Production.** Vercel stores one entry per key with a target list;
> removing the Preview target through the CLI can remove the entire entry,
> including Production. Use the dashboard's per-environment editor, or
> duplicate the value into a separate Preview entry first. This exact
> mistake caused a production boot failure on 2026-11-20 (`ENV` and the
> database/Redis/LLM values disappeared from Production). The previous
> deployment kept serving because Vercel pins a deployment's environment at
> build time.

| Var | Vercel value | Why |
|---|---|---|
| `BACKGROUND_TICKERS` | `false` | Both the reminder ticker and the notify drain loop assume a long-lived process. Vercel destroys it between requests, so they would never fire and Supabase owns delivery instead. **Getting this wrong means reminders silently never fire.** |
| `ENV` | `production` | Strict enum; production tightens CORS and rejects a wildcard origin. |
| `RUN_MIGRATIONS` | `false` | **Must be false on Vercel.** Preview and production refuse to boot with it true. Production DDL runs through the manual `db-migrate` workflow. |
| `DATABASE_URL` | Supavisor **6543** TX pooler | Transaction mode. Add `?pgbouncer=true`. Must connect as `app_runtime`; a superuser or BYPASSRLS role fails the boot check. |
| `DB_USE_SIMPLE_PROTOCOL` | `true` | 6543 is pgbouncer; no extended-query prepared statements. Required. |
| `DB_POOL_MIN` | `0` | No warm connections — there is no long-lived process |
| `DB_POOL_MAX` | `2` | **Per instance.** The default of 20 would let 20 concurrent lambdas open 400 connections into Supabase |
| `TRUSTED_PROXY_CIDRS` | unset | Vercel overwrites `X-Forwarded-For`; `internal/api/clientip.go` trusts it when `VERCEL=1`, so per-visitor rate-limit buckets work without a CIDR list. Set this only for a self-managed proxy in front of a non-Vercel deploy. |
| `ALLOW_ORIGINS` | optional fallback only | CORS is normally DB-backed through `frontend_origins` + `origin_suffix_rules`. Keep this only as an emergency/debug fallback; `*` in prod defeats the `ENV=production` tightening. |
| `ALLOW_REMOTE_DB` | unset | Staging only. Production is remote by definition; Preview carries no database at all. |
| `DATABASE_ADMIN_URL` | direct **5432**, superuser | Admin endpoints (and `cmd/migrate`). Required when `ENV=production`. |
| `REDIS_URL` | `rediss://…` **in ap-southeast-1 (Singapore)** | Cache, quota, rate limits — every request pays 1–3 sequential Redis round trips, so a cross-region Upstash adds 100–500ms per request. Same-region (`sin1` ↔ ap-southeast-1) keeps them at ~1–2ms. |
| `LLM_BASE_URL` / `LLM_API_KEY` / `LLM_MODEL` | as `.env` | |
| `ADMIN_API_KEY` | 64 hex chars | |
| `REPORT_WORKER_TOKEN` | 64 hex chars | Enables the deferred report worker endpoint (`POST /v1/internal/report-jobs/build`, migration 064). Must match the Supabase Vault secret `report_worker_token` used by the `report-jobs-tick` cron. Empty disables the endpoint (503). |
| `SENTRY_DSN` | optional | Empty disables Sentry entirely |

## Deploy

```bash
npx vercel link      # once
npx vercel env add BACKGROUND_TICKERS production      # "false"
npx vercel env add RUN_MIGRATIONS production          # "false"
npx vercel env add RUN_MIGRATIONS preview             # "false"
npx vercel env add RUN_MIGRATIONS development         # "false"
npx vercel deploy --prod
```

Production DDL is a separate, auditable step:

```bash
gh workflow run db-migrate.yml -f ref=main
```

The workflow is gated by the GitHub `production` Environment (add required
reviewers). It applies platform migrations and every `projects/<slug>/`
migration, then prints both ledgers. Never run `cmd/migrate` from a laptop
against production; the local `.env` has no remote credentials and
`ENV=development` refuses them.

Fluid Compute defaults to a 300s cap on every plan (Hobby maximum).
It comfortably covers one agent turn (`adkrunner.TurnTimeout` 80s +
10s flush slack inside the server's `WriteTimeout`).

## What you will feel

- **1–3s cold start per request.** Each inbound request boots a fresh
  process: Sentry init, config, two PG pools, Redis.
  If `Fluid Active CPU` ever becomes the constraint, the lever
  is that boot work, not the request path.
- **No migration check on cold start.** `RUN_MIGRATIONS=false` means boot
  skips the ledger entirely. Production migrations run through the manual
  `db-migrate` workflow, which applies platform migrations and each
  `projects/<slug>/migrations/` inside its schema, and verifies every ledger
  checksum. A preview never reaches a database at all.

Neither is a correctness problem. Both are why `BACKGROUND_TICKERS=false` and
Supabase-side delivery are required rather than optional.

## Scheduled jobs & backups

| Where | Job | Schedule (UTC) | What |
|---|---|---|---|
| Supabase pg_cron | `reminder-tick` | `*/2` | probe-gated Edge invocation (only when a reminder is due) |
| Supabase pg_cron | `reaper-tick` | `*/5` | hands back orphaned claims (older than 10 min) |
| Supabase pg_cron | `retention-tick` | `17 20` (03:17 WIB) | batched deletes: sessions 7d, sent reminders 90d, dead 30d, usage 400d, archived notify 14d, events 180d, report backstop |
| Supabase pg_cron | `report-jobs-tick` | `* * * * *` | deferred report builds (064): reaps stale `working` jobs, then posts ONE build to the backend worker. No-op until the Vault secrets `report_worker_url` + `report_worker_token` exist (see `ops/schedule-report-jobs.sql`). |
| GitHub Actions | `backup.yml` | `0 18` (01:00 WIB) | pg_dump of `public`+`private` → artifact (30d) → restore-smoke into postgres:17 |
| GitHub Actions | `smoke.yml` | `*/30` | backend `/healthz` + frontend probe (3 attempts each) |
| GitHub Actions | `security.yml` | Mondays 06:17 | govulncheck + gitleaks |

- pg_cron evaluates schedules in the **database** timezone (UTC on
  Supabase). The first retention install said "03:17 local" and actually
  fired at 03:17 UTC; corrected 2026-10-19.
- GitHub scheduled runs can queue late (tens of minutes); that is normal
  and does not affect correctness.
- Backup credential: migration 062 creates `backup_reader`
  (`LOGIN BYPASSRLS` + `pg_read_all_data`). BYPASSRLS is load-bearing:
  every tenant table is FORCE RLS, and a NOBYPASSRLS reader would dump
  **empty tables**. The password lives only in the GitHub secret
  `BACKUP_DATABASE_URL` (pooler session mode, `sslmode=require`). Rotate
  like any privileged credential.
- Dump scope is `public`+`private` plus every `project_*` schema. Platform schemas
  (auth/storage/vault) are Supabase-managed and covered by the platform's
  daily WAL-G backups; including them breaks portable restores and drags
  platform PII (auth password hashes) into the artifact. Platform PITR is
  off (`pitr_enabled=false`); the nightly dump is the independent,
  off-platform copy.
- Verify: `gh run list --workflow backup.yml --limit 3`, then download
  `db-backup` and restore into a scratch database
  (`gunzip -c *.sql.gz | psql <scratch>`); the smoke job does exactly
  this on every run, including a schema_migrations count and row probes.
