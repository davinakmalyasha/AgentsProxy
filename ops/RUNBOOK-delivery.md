# Supabase hosted delivery — setup

One-time per environment. Local dev (Laragon) does **not** need any of
this: the Go ticker owns delivery there because the process stays alive
between ticks.

## What this is for

On a long-running host (Laragon, VPS, Railway) the Go process runs a 60s
ticker and delivers reminders itself. On serverless (Vercel) it cannot —
the function is destroyed between requests, so the ticker never ticks and
an 8am reminder silently never fires. Supabase owns the clock there.

Both drivers claim rows the same way, so enabling both is safe; only one
is active per environment.

| | Long-running host | Serverless |
|---|---|---|
| `BACKGROUND_TICKERS` | `true` (default) | `false` |
| Reminder delivery | Go ticker, `internal/reminder` | `pg_cron` → `deliver-reminders` |
| Notification drain | Go ticker, `internal/notify` | DB Webhook → `deliver-notification`, + cron backstop |
| Stale-claim reaper | Go boot path | `pg_cron` → `reap_stale_claims` (pure SQL) |

## Prerequisites

1. Migrations 029–031 applied (the server does this on boot):

   ```bash
   DATABASE_URL=... DATABASE_ADMIN_URL=... go run ./cmd/server
   ```

2. Two Edge Functions deployed:

   ```bash
   supabase functions deploy deliver-reminders
   supabase functions deploy deliver-notification
   ```

3. Three secrets. Two in Supabase Vault (read by `pg_cron`/`pg_net`),
   one as an Edge Function secret (used by Deno):

   ```sql
   -- vault: the Edge Functions base URL, no trailing /functions/v1
   select vault.create_secret('<project-ref>.functions.supabase.co',
                              'delivery_functions_url');

   -- vault: bearer token for the internal functions. The anon key works
   -- when the functions run with verify_jwt = false. Otherwise use a
   -- dedicated signed JWT.
   select vault.create_secret('<token>', 'delivery_auth_token');
   ```

   ```bash
   # Edge Function secrets (read by Deno at request time)
   supabase secrets set \
     TELEGRAM_VAULT_KEY=<64 hex chars - same value as the Go env var> \
     DELIVERY_DATABASE_URL='postgresql://delivery_caller:<password>@db.<ref>.supabase.co:5432/postgres'
   ```

## The `delivery_caller` password

`delivery_caller` is created by migration 031 as `LOGIN NOBYPASSRLS` with
**no password** — secrets never belong in a migration. Set it once:

```sql
alter role delivery_caller password '<generated>';
```

That role can only `EXECUTE` the `private.claim_*` functions. It has no
`USAGE` on `public` and no table grants, so a dump of the role is not a
route to tenant data.

## Wire it up

```bash
psql "$DATABASE_ADMIN_URL" -f ops/schedule-delivery.sql
```

This installs three `pg_cron` jobs (`reminder-tick` every 2 min,
`reaper-tick` and `notify-drain` every 5 min) and one INSERT trigger on
`notify_jobs` for instant delivery. Both reminder and drain ticks are
gated on an `exists()` probe, so an idle system spends one indexed query
per tick and no Edge Function invocations at all.

## Verify

```sql
-- jobs installed
select jobname, schedule, active from cron.job order by jobname;

-- did they actually run?
select jobname, status, start_time from cron.job_run_details
 where jobid in (select jobid from cron.job) order by start_time desc limit 10;

-- webhook calls (pg_net log)
select * from net._http_response order by id desc limit 5;
```

End to end:

```sql
-- with BACKGROUND_TICKERS=false on the running app
insert into agent_reminders (tenant_id, chat_id, text, remind_at)
values ('centrum', '<chat_id>', 'DELIVERY SMOKE TEST', now() + interval '2 minutes');
```

A Telegram message should arrive within ~2 minutes. Then:

```sql
select text, status, attempts, claimed_at
  from agent_reminders where text = 'DELIVERY SMOKE TEST';
-- status = 'sent', claimed_at = null
```

For notifications, book a court as owner and watch the webhook drain it
without waiting for the 5-minute backstop.

## Free-tier note

Supabase free projects **pause after 7 days of inactivity**, and a
paused project stops running `pg_cron` — reminders silently stop. The
proxy writes to the database continuously during real use, so it will not
idle, but an abandoned demo environment will need a manual **Restore** and
will lose any reminders scheduled during the pause. If this becomes a
problem, a nightly `pg_cron` job is not enough on its own (a paused
project runs nothing); the fix is an external keep-alive ping or the paid
plan.
