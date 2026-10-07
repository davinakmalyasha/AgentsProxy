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

| Var | Vercel value | Why |
|---|---|---|
| `BACKGROUND_TICKERS` | `false` | Both the reminder ticker and the notify drain loop assume a long-lived process. Vercel destroys it between requests, so they would never fire and Supabase owns delivery instead. **Getting this wrong means reminders silently never fire.** |
| `DATABASE_URL` | Supavisor **6543** TX pooler | Transaction mode. Add `?pgbouncer=true` |
| `DB_USE_SIMPLE_PROTOCOL` | `true` | 6543 is pgbouncer; no extended-query prepared statements. Required. |
| `DB_POOL_MIN` | `0` | No warm connections — there is no long-lived process |
| `DB_POOL_MAX` | `2` | **Per instance.** The default of 20 would let 20 concurrent lambdas open 400 connections into Supabase |
| `TRUSTED_PROXY_CIDRS` | Vercel egress ranges | Without it `internal/api/clientip.go` ignores `X-Forwarded-For`, every client buckets as one IP, and you rate-limit yourself into `429`s |
| `ALLOW_ORIGINS` | comma-separated client origins | `*` in prod defeats the CORS tightening that `ENV=production` enables |
| `ENV` | `production` | Tightens CORS |
| `DATABASE_ADMIN_URL` | direct **5432**, superuser | Migrations + admin endpoints. Runs on every cold start, so keep the DSN fast |
| `REDIS_URL` | `rediss://…` | Cache, quota, rate limits |
| `LLM_BASE_URL` / `LLM_API_KEY` / `LLM_MODEL` | as `.env` | |
| `ADMIN_API_KEY` | 64 hex chars | |
| `SENTRY_DSN` | optional | Empty disables Sentry entirely |

## Deploy

```bash
npx vercel link      # once
npx vercel env add BACKGROUND_TICKERS production      # "false"
npx vercel env add RUN_MIGRATIONS production          # "false" (migrations run as a deploy step: make migrate)
npx vercel deploy --prod
```

Fluid Compute defaults to a 300s cap on every plan (Hobby maximum).
It comfortably covers one agent turn (`adkrunner.TurnTimeout` 80s +
10s flush slack inside the server's `WriteTimeout`).

## What you will feel

- **1–3s cold start per request.** Each inbound request boots a fresh
  process: Sentry init, config, two PG pools, the migration check, Redis.
  If `Fluid Active CPU` ever becomes the constraint, the lever
  is that boot work, not the request path.
- **The migration check runs on every cold start.**
  `pg_advisory_xact_lock(734561)` in `internal/db/migrate.go` makes that
  safe under concurrency — the losers see the migration already applied —
  but it is a real round trip each time.

Neither is a correctness problem. Both are why `BACKGROUND_TICKERS=false` and
Supabase-side delivery are required rather than optional.
