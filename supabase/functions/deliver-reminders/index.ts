// Drain the durable reminder queue (serverless driver).
//
// Triggered by pg_cron `reminder-tick` (ops/schedule-delivery.sql).
// The Go ticker (internal/reminder, BACKGROUND_TICKERS=true) is the twin
// driver for long-running hosts; mutual exclusion comes from the claim
// (status='sending' + claimed_at), never from scheduler choice.
//
// Claim protocol mirrors the notify function exactly:
//   private.claim_due_reminders(limit) → send → private.complete_reminder
//   on success ('sent'), private.fail_reminder on transient failure
//   (attempts+1, 'dead' past 5 — the SQL twin of bumpAttempts).
//
// Quarantine is enforced in SQL: claim_due_reminders skips tenants in
// tenant_quarantine, so a fenced tenant's reminders wait regardless of
// which driver runs.
//
// Today the only sender is the zero-credential wa.me deep link, so
// nothing user-visible happens. Swapping in a real transport (PWA push)
// means replacing sendOne() — the queue and claim plumbing is unchanged.
import "jsr:@supabase/functions-js/edge-runtime.d.ts";
import postgres from "npm:postgres";

/** Mirrors reminder.maxAttempts (internal/reminder/reminder.go). */
const MAX_ATTEMPTS = 5;
const BATCH = 50;
const SEND_TIMEOUT_MS = 8_000;

interface ClaimedReminder {
  id: string;
  tenant_id: string;
  chat_id: string;
  text: string;
  attempts: number;
  claimed_at: string;
}

/**
 * Zero-credential default, identical to the Go scheduler with no sender:
 * WhatsApp delivery needs a provider, PWA push needs the subscription
 * plumbing. Until then we emit a wa.me deep link. Replace this one
 * function to go live.
 */
async function sendOne(to: string, text: string): Promise<void> {
  const link = `https://wa.me/${encodeURIComponent(to)}?text=${encodeURIComponent(text)}`;
  console.log(JSON.stringify({ event: "reminder_wa_me", to, link }));
  await Promise.resolve();
}

function withTimeout<T>(p: Promise<T>, ms: number): Promise<T> {
  return Promise.race([
    p,
    new Promise<never>((_, reject) =>
      setTimeout(() => reject(new Error("send timeout")), ms)
    ),
  ]);
}

Deno.serve(async () => {
  const dsn = Deno.env.get("DELIVERY_DATABASE_URL");
  if (!dsn) {
    return new Response("DELIVERY_DATABASE_URL not set", { status: 500 });
  }
  const sql = postgres(dsn, { max: 1, prepare: false });

  const report = { claimed: 0, sent: 0, dead: 0, failed: 0 };

  try {
    // claim_due_reminders RETURNS SETOF jsonb, one `j` column per row.
    const rows = await sql<{ j: ClaimedReminder }[]>`
      select j from private.claim_due_reminders(${BATCH}) as j
    `;
    const batch = rows.map((r) =>
      (typeof r.j === "string" ? JSON.parse(r.j) : r.j) as ClaimedReminder
    );
    report.claimed = batch.length;

    for (const job of batch) {
      try {
        await withTimeout(sendOne(job.chat_id, job.text), SEND_TIMEOUT_MS);
        await sql`select private.complete_reminder(${job.id}::uuid, ${job.claimed_at}::timestamptz, 'sent')`;
        report.sent++;
      } catch (err) {
        console.error(JSON.stringify({
          event: "reminder_send_failed",
          id: job.id,
          attempt: job.attempts,
          err: err instanceof Error ? err.message : String(err),
        }));
        // fail_reminder returns the post-bump status; 'dead' past max.
        const res = await sql<{ status: string }[]>`
          select private.fail_reminder(${job.id}::uuid, ${job.claimed_at}::timestamptz, ${MAX_ATTEMPTS}) as status
        `;
        if (res[0]?.status === "dead") {
          report.dead++;
        }
        report.failed++;
      }
    }
  } catch (err) {
    console.error(JSON.stringify({
      event: "reminder_driver_error",
      err: err instanceof Error ? err.message : String(err),
    }));
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
