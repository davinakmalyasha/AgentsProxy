// Drain due reminders over Web Push.
//
// Mirrors deliver-notification (same claim/complete/fail shape, same
// report envelope), but each claimed row fans out to that recipient's
// live push subscriptions instead of one phone number:
//
//   1. claim a batch via private.claim_due_reminders (quarantine-aware,
//      tenant-scoped by the claim itself — the function sees all tenants
//      because pg_cron has no tenant context, same as the Go ticker on
//      the admin pool).
//   2. load subscriptions for (tenant_id, recipient_ref); none →
//      release the claim WITHOUT failing (missing addressability is
//      not a delivery failure — mirrors Scheduler errNoSubscriptions).
//      There is no release RPC that preserves attempts... fail_reminder
//      bumps, so instead: complete_reminder(..., 'pending') is NOT a
//      valid status; use a direct UPDATE back to pending via the
//      delivery_caller table rights? delivery_caller has NO table
//      grants by design. Therefore: call fail_reminder ONLY on real
//      send failures; for no-subscriptions, leave the row claimed and
//      let reap_stale_claims hand it back after the window. A claimed
//      row is invisible to other drivers, so nothing double-sends; the
//      reaper restores it. Slow (10 min) but safe and correct.
//   3. push per subscription (VAPID keys from env); 410/404 prunes the
//      subscription row; any 2xx → complete_reminder 'sent'; all failed
//      → fail_reminder (attempts ladder to dead at 5, same as Go).
//
// Trigger: pg_cron every 2 min behind the any_reminders_due() probe
// (ops/schedule-delivery.sql) — NOT YET SCHEDULED. Deploy first,
// schedule with the pg_cron proposal.
//
// Env: DELIVERY_DATABASE_URL (delivery_caller credential), VAPID_PUBLIC_KEY,
// VAPID_PRIVATE_KEY, VAPID_SUBJECT (mailto/contact).
import "jsr:@supabase/functions-js/edge-runtime.d.ts";
import postgres from "npm:postgres";
import { encryptPush, vapidAuth } from "./crypto.ts";

const BATCH = 20;
const MAX_ATTEMPTS = 5;
const SEND_TIMEOUT_MS = 8_000;

interface ClaimedReminder {
  id: string;
  tenant_id: string;
  chat_id: string;
  text: string;
  attempts: number;
  claimed_at: string;
}

interface PushSub {
  endpoint: string;
  p256dh: string;
  auth: string;
}

async function sendPush(
  sub: PushSub,
  payload: Uint8Array<ArrayBuffer>,
  vapid: { publicKey: string; privateKey: string; subject: string },
): Promise<number> {
  const aud = new URL(sub.endpoint).origin;
  const auth = await vapidAuth(vapid.publicKey, vapid.privateKey, vapid.subject, aud);
  const body = await encryptPush({ p256dh: sub.p256dh, auth: sub.auth }, payload);
  const ctrl = new AbortController();
  const t = setTimeout(() => ctrl.abort(), SEND_TIMEOUT_MS);
  try {
    const res = await fetch(sub.endpoint, {
      method: "POST",
      headers: {
        Authorization: `vapid t=${auth}, k=${vapid.publicKey}`,
        "Content-Encoding": "aes128gcm",
        TTL: "86400",
      },
      body: body as BodyInit,
      signal: ctrl.signal,
    });
    return res.status;
  } finally {
    clearTimeout(t);
  }
}

// NOTE: this Edge driver exists for serverless ticks but stays
// UNSCHEDULED until the pg_cron proposal lands. The Go PushSender
// (internal/reminder) is the primary driver. Crypto (crypto.ts) is
// round-trip verified in Deno; the claim/complete/fail plumbing below
// mirrors deliver-notification.
function reminderPayload(text: string): Uint8Array<ArrayBuffer> {
  return new TextEncoder().encode(
    JSON.stringify({ title: "Centrum Badminton", body: `⏰ Reminder: ${text}`, url: "/app" }),
  );
}

Deno.serve(async () => {
  const dsn = Deno.env.get("DELIVERY_DATABASE_URL");
  const vapidPublic = Deno.env.get("VAPID_PUBLIC_KEY") ?? "";
  const vapidPrivate = Deno.env.get("VAPID_PRIVATE_KEY") ?? "";
  const vapidSubject = Deno.env.get("VAPID_SUBJECT") ?? "mailto:admin@localhost";
  if (!dsn) {
    return new Response("DELIVERY_DATABASE_URL not set", { status: 500 });
  }
  const sql = postgres(dsn, { max: 1, prepare: false });
  const report = { claimed: 0, sent: 0, held: 0, failed: 0, dead: 0 };

  try {
    const rows = await sql<{ j: ClaimedReminder }[]>`
      select j from private.claim_due_reminders(${BATCH}) as j`;
    const batch = rows.map((r) =>
      (typeof r.j === "string" ? JSON.parse(r.j) : r.j) as ClaimedReminder
    );
    report.claimed = batch.length;

    for (const rem of batch) {
      // The claim payload has no recipient_ref (031-era shape kept for
      // compat): resolve via the row for routing.
      const got = await sql<{ recipient_ref: string }[]>`
        select recipient_ref from agent_reminders where id = ${rem.id}::uuid`;
      const ref = got[0]?.recipient_ref ?? "";
      if (!ref) {
        // Unroutable pre-push row: hold by doing nothing — the reaper
        // hands it back after the window (see header note on why we
        // must not fail_reminder here).
        report.held++;
        continue;
      }
      const subs = await sql<PushSub[]>`
        select endpoint, p256dh, auth from push_subscriptions
         where tenant_id = ${rem.tenant_id} and user_ref = ${ref}`;
      if (subs.length === 0) {
        report.held++;
        continue;
      }
      let sent = false;
      for (const sub of subs) {
        try {
          const st = await sendPush(sub, reminderPayload(rem.text), {
            publicKey: vapidPublic,
            privateKey: vapidPrivate,
            subject: vapidSubject,
          });
          if (st === 404 || st === 410) {
            await sql`delete from push_subscriptions
               where tenant_id = ${rem.tenant_id} and endpoint = ${sub.endpoint}`;
            continue;
          }
          if (st >= 200 && st < 300) {
            sent = true;
            await sql`update push_subscriptions set last_used_at = now()
               where tenant_id = ${rem.tenant_id} and endpoint = ${sub.endpoint}`;
          }
        } catch (err) {
          console.error(JSON.stringify({ event: "push_send_failed", id: rem.id, err: String(err) }));
        }
      }
      if (sent) {
        await sql`select private.complete_reminder(${rem.id}::uuid, ${rem.claimed_at}::timestamptz, 'sent')`;
        report.sent++;
      } else {
        const done = await sql<{ fail_reminder: string }[]>`
          select private.fail_reminder(${rem.id}::uuid, ${rem.claimed_at}::timestamptz, ${MAX_ATTEMPTS}) as fail_reminder`;
        if (done[0]?.fail_reminder === "dead") report.dead++;
        else report.failed++;
      }
    }
  } catch (err) {
    console.error(JSON.stringify({ event: "reminder_driver_error", err: String(err) }));
    await sql.end({ timeout: 1 });
    return new Response(JSON.stringify({ ...report, driver_error: true }), {
      status: 500,
      headers: { "Content-Type": "application/json" },
    });
  }

  await sql.end({ timeout: 5 });
  return new Response(JSON.stringify(report), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
});
