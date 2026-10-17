// Drain due reminders over Web Push.
//
// Mirrors deliver-notification (same claim/complete/fail shape, same
// report envelope), but each claimed row fans out to that recipient's
// live push subscriptions instead of one phone number.
//
// All database access goes through owner-scoped SECURITY DEFINER RPCs
// (migration 059): delivery_caller holds EXECUTE on them and NO table
// grants, so this file must never touch a table directly.
//
//   1. claim a batch via private.claim_due_reminders (quarantine-aware,
//      cross-tenant by design — pg_cron has no tenant context, same as
//      the Go ticker on the admin pool). The payload carries tenant_id,
//      recipient_ref, text and claimed_at.
//   2. resolve subscriptions via private.push_subscriptions_for; none
//      (or an empty recipient_ref on a pre-push row) → the claim is
//      handed back to pending WITHOUT burning attempts via
//      private.release_reminder — missing addressability is not a
//      delivery failure (mirrors Go Scheduler errNoSubscriptions →
//      releaseClaim). A released row is due again immediately, so the
//      next tick retries it; nothing double-sends because the claim
//      protocol keeps rows invisible while 'sending'.
//   3. push per subscription (VAPID keys from env); 404/410 →
//      private.push_subscription_gone prunes the endpoint; any 2xx →
//      private.push_subscription_touch stamps last_used_at and
//      complete_reminder 'sent'; all failed → fail_reminder (attempts
//      ladder to dead at 5, same as Go).
//
// Trigger: pg_cron every 2 min behind the any_reminders_due() probe
// (ops/schedule-reminders.sql, installed on prod 2026-10-15). The Go
// PushSender (internal/reminder) is the long-running-host driver; both
// share the atomic claim and are safe to run together.
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
  recipient_ref: string;
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
  // Failure localization for ops: the driver_error response (stored by
  // pg_net in net._http_response) says which phase threw, without
  // exposing raw error text on a publicly invokable endpoint.
  let stage = "claim";

  try {
    const rows = await sql<{ j: ClaimedReminder }[]>`
      select j from private.claim_due_reminders(${BATCH}) as j`;
    const batch = rows.map((r) =>
      (typeof r.j === "string" ? JSON.parse(r.j) : r.j) as ClaimedReminder
    );
    report.claimed = batch.length;

    stage = "deliver";
    const release = async (rem: ClaimedReminder) => {
      await sql`select private.release_reminder(${rem.id}::uuid, ${rem.claimed_at}::timestamptz)`;
    };

    for (const rem of batch) {
      const ref = rem.recipient_ref ?? "";
      if (!ref) {
        await release(rem);
        report.held++;
        continue;
      }
      const subs = await sql<PushSub[]>`
        select endpoint, p256dh, auth
          from private.push_subscriptions_for(${rem.tenant_id}, ${ref})`;
      if (subs.length === 0) {
        await release(rem);
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
            await sql`select private.push_subscription_gone(${rem.tenant_id}, ${sub.endpoint})`;
            continue;
          }
          if (st >= 200 && st < 300) {
            sent = true;
            await sql`select private.push_subscription_touch(${rem.tenant_id}, ${sub.endpoint})`;
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
    console.error(JSON.stringify({ event: "reminder_driver_error", stage, err: String(err) }));
    await sql.end({ timeout: 1 });
    return new Response(JSON.stringify({ ...report, driver_error: true, stage }), {
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
