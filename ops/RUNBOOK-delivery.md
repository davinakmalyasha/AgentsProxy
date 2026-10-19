# Supabase hosted delivery — setup (PWA Web Push)

One-time per environment. Local dev (Laragon) does **not** need any of
this: the Go ticker owns delivery there because the process stays alive
between ticks.

## What this is for

On a long-running host (Laragon, VPS, Railway) the Go process runs a
ticker and delivers reminders itself. On serverless (Vercel) it cannot —
the function is destroyed between requests, so an 8am reminder silently
never fires. Supabase owns the clock there.

Both drivers claim rows atomically, so they can coexist; on Vercel the Go
ticker is off (`BACKGROUND_TICKERS=false`) and the Edge driver is the
only one.

| | Long-running host | Serverless (prod) |
|---|---|---|
| `BACKGROUND_TICKERS` | `true` (default) | `false` |
| Reminder delivery | Go ticker (`internal/reminder`, raw SQL claim) | `pg_cron` → `deliver-reminders` Edge Function (installed 2026-10-15) |
| Stale-claim reaper | Go boot path | `pg_cron` → `reap_stale_claims` |
| Notify drain | Go ticker | **not installed** — `deliver-notification` is never deployed; `ops/schedule-delivery.sql` is the historical (pre-push) design |

## Architecture in one paragraph

`deliver-reminders` runs as `delivery_caller`: `EXECUTE` on the driver
RPCs and **no table grants**. Every table touch goes through
`private.*` functions owned by `delivery_worker` (`BYPASSRLS`): claim →
look up subscriptions (`push_subscriptions_for`) → Web Push (VAPID) →
complete / fail / release. Claims stamp **millisecond** precision (060)
because postgres.js truncates ISO timestamps when binding; the same
`claimed_at` token scopes every follow-up write. Payload encryption is
RFC 8291/8188 and is pinned by `crypto_test.ts` against an independent
RFC-written decryptor — FCM accepts undecryptable bodies, so only a real
device test catches crypto bugs.

## Prerequisites

1. Migrations applied (server boot, or `make migrate`). Delivery surface:
   057 (registry + drivers), 059 (RPCs), 060 (ms precision).
2. `pg_cron` + `pg_net` enabled on the project.
3. Edge Function deployed:

   ```bash
   npx supabase functions deploy deliver-reminders \
     --project-ref <ref> --no-verify-jwt --use-api
   ```

   `--use-api` bundles server-side (no Docker). Keep `verify_jwt=false`:
   the claim protocol, not the URL, is the security boundary.

4. Edge secrets:

   ```bash
   supabase secrets set \
     DELIVERY_DATABASE_URL='postgresql://delivery_caller.<ref>:<password>@aws-0-<region>.pooler.supabase.com:5432/postgres?sslmode=require' \
     VAPID_PUBLIC_KEY='<raw 65-byte b64url>' \
     VAPID_PRIVATE_KEY='<raw 32-byte b64url>' \
     VAPID_SUBJECT='mailto:ops@example.com'
   ```

   - Session-mode pooler (5432), tenant-qualified username
     `delivery_caller.<ref>`.
   - VAPID keys: raw uncompressed P-256 public (65 bytes) and raw scalar
     (32 bytes), unpadded base64url. The frontend must ship the **same**
     public key as `NEXT_PUBLIC_VAPID_PUBLIC_KEY`.
   - Secret changes reach new isolates within seconds; if a change looks
     ignored, redeploy the function to force fresh isolates.

5. `delivery_caller` password (out-of-band; never in a migration):

   ```sql
   alter role delivery_caller password '<generated>';
   ```

   The role can only `EXECUTE` the delivery RPCs — no table grants, no
   `USAGE` on `public`.

6. Schedules:

   ```bash
   psql "$DATABASE_ADMIN_URL" -f ops/schedule-reminders.sql
   psql "$DATABASE_ADMIN_URL" -f ops/retention.sql   # pruning, separate concern
   ```

   `reminder-tick` (`*/2`) and `reaper-tick` (`*/5`) are probe-gated: an
   idle system spends one indexed query per tick and zero Edge
   invocations.

7. Frontend: set `NEXT_PUBLIC_VAPID_PUBLIC_KEY` (same public key), then
   staff enable notifications from the header bell. The browser posts the
   subscription through the app's same-origin shim
   (`/api/push/subscriptions`) because the session cookie is httpOnly —
   client code cannot read it.

## Verify

```sql
select jobname, schedule, active from cron.job order by jobname;

select jobname, status, start_time from cron.job_run_details
 where jobid in (select jobid from cron.job) order by start_time desc limit 10;

select created, status_code, content from net._http_response
 order by created desc limit 5;

select tenant_id, user_ref, last_used_at from push_subscriptions;
```

With a due reminder: `reminder-tick` rows say `1 row` and
`net._http_response` shows `200 {"claimed":1,"sent":1,...}`.

Function logs (dashboard) record:

- `reminder_driver_error` with a `stage` field — `claim` = DB/credentials,
  `deliver` = send loop. A `403 invalid JWT provided` from FCM means the
  VAPID pair does not match, never a subscription problem.
- `push_send_status` for any non-2xx push-service response (404/410 prune
  the endpoint instead).
- `push_send_failed` for transport errors (DNS, timeouts).

A recipient with no subscriptions is *held*, not failed: the claim is
released without burning an attempt (mirrors the Go scheduler).

## Notes

- Cron schedules are in the **database** timezone (UTC on Supabase).
  `retention-tick` runs at 20:17 UTC = 03:17 WIB.
- The reaper releases claims older than 10 minutes — many times the
  per-row send budget, so a slow-but-alive driver is never robbed.
- Free-tier projects pause after 7 days of inactivity, and a paused
  project stops `pg_cron`. Real use keeps it alive; an abandoned demo
  needs a manual restore and loses reminders scheduled during the pause.
- Backups: see `RUNBOOK-vercel.md` → "Scheduled jobs & backups".
