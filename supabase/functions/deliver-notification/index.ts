// Drain the durable notification queue.
//
// Triggered two ways (ops/schedule-delivery.sql):
//   1. A Database Webhook on INSERT into notify_jobs - instant, so a
//      booking confirmation does not wait for a cron tick.
//   2. A pg_cron backstop every 5 minutes - covers the case where the
//      webhook's HTTP call never landed (pg_net is best-effort).
//
// pg_cron on its own would be up to 5 minutes late, which is why the
// webhook exists. The cron is the safety net, not the primary path.
//
// Mutual exclusion with the Go driver's Drain() comes from the
// visibility timeout on notify_jobs, not from a claim flag: whoever
// reads a row first pushes its vt forward, and the other driver cannot
// see it until vt expires. A driver that dies before delete/archive
// makes the row reappear rather than losing it.
//
// Today the only sender is the zero-credential wa.me deep link, so
// nothing user-visible happens. Swapping in Meta/Twilio means replacing
// sendOne() - the queue and claim plumbing is unchanged.
//
// DONE: Vercel/Supabase split / STANDARDS A, E
import "jsr:@supabase/functions-js/edge-runtime.d.ts";
import postgres from "npm:postgres";

/** Mirrors notify.maxAttempts (internal/notify/queue.go). */
const MAX_ATTEMPTS = 3;
/** Mirrors notify.vtInterval. */
const VT_SECONDS = 60;
const BATCH = 20;
const SEND_TIMEOUT_MS = 10_000;

interface ClaimedJob {
  id: string;
  to: string;
  text: string;
  read_ct: number;
}

/**
 * Zero-credential default, identical to notify.WaMeSender: WhatsApp
 * Business messaging needs a provider (Meta Cloud API / Twilio) plus
 * business verification, so until then we emit a wa.me deep link.
 * Replace this one function to go live.
 */
async function sendOne(to: string, text: string): Promise<void> {
  const link = `https://wa.me/${encodeURIComponent(to)}?text=${encodeURIComponent(text)}`;
  console.log(JSON.stringify({ event: "notify_wa_me", to, link }));
  await Promise.resolve();
}

Deno.serve(async () => {
  const dsn = Deno.env.get("DELIVERY_DATABASE_URL");
  if (!dsn) {
    return new Response("DELIVERY_DATABASE_URL not set", { status: 500 });
  }
  const sql = postgres(dsn, { max: 1, prepare: false });

  const report = { claimed: 0, sent: 0, archived: 0, failed: 0 };

  try {
    // The claim functions RETURNS SETOF jsonb, so each row is a single
    // column `j` holding the payload.
    const rows = await sql<{ j: ClaimedJob }[]>`
      select j from private.claim_notifications(${BATCH}, ${VT_SECONDS}::int) as j
    `;
    const batch = rows.map((r) =>
      (typeof r.j === "string" ? JSON.parse(r.j) : r.j) as ClaimedJob
    );
    report.claimed = batch.length;

    for (const job of batch) {
      try {
        await sendOne(job.to, job.text);
        await sql`select private.delete_notification(${job.id}::bigint)`;
        report.sent++;
      } catch (err) {
        console.error(JSON.stringify({
          event: "notify_send_failed",
          id: job.id,
          to: job.to,
          // read_ct was incremented by the claim, so it IS the attempt.
          attempt: job.read_ct,
          err: err instanceof Error ? err.message : String(err),
        }));
        if (job.read_ct >= MAX_ATTEMPTS) {
          // Archived, not dropped: the Go driver does the same so a
          // failed notification stays inspectable.
          await sql`select private.archive_notification(${job.id}::bigint)`;
          report.archived++;
        }
        report.failed++;
      }
    }
  } catch (err) {
    console.error(JSON.stringify({
      event: "notify_driver_error",
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
