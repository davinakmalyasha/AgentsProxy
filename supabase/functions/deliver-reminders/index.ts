// Deliver due agent reminders to Telegram.
//
// Scheduled by pg_cron (ops/schedule-delivery.sql) instead of the
// in-process Go ticker, because on serverless (Vercel) the process is
// destroyed between requests and the 60s ticker never gets a tick.
//
// Claims rows through private.claim_due_reminders(), which flips
// status 'pending' -> 'sending' atomically. The Go ticker claims the
// same way, so the two drivers can never deliver the same row twice -
// exactly one of them wins each claim.
//
// Bot tokens are returned as BASE64 CIPHERTEXT and decrypted here with
// the vault key, so the key never has to live in Postgres and a
// database dump is not enough to impersonate a tenant's bot.
//
// Database access uses the delivery_caller role: EXECUTE on the private
// claim functions and nothing else. No table grants, no RLS bypass
// (the SECURITY DEFINER functions carry that, owned by
// delivery_worker).
//
// DONE: Vercel/Supabase split / STANDARDS A, D, E, F
import "jsr:@supabase/functions-js/edge-runtime.d.ts";
import postgres from "npm:postgres";

/** tgvault wire format: 0x01 || nonce(12) || ciphertext||tag (AES-256-GCM). */
const VAULT_VERSION = 0x01;
const NONCE_LEN = 12;

/** Mirrors reminder.maxAttempts (internal/reminder/reminder.go). */
const MAX_ATTEMPTS = 5;

/** Mirrors the per-row send budget in deliver() (8s). */
const SEND_TIMEOUT_MS = 8_000;
const BATCH = 20;
const TELEGRAM_API = "https://api.telegram.org";

interface ClaimedReminder {
  id: string;
  tenant_id: string;
  chat_id: string;
  text: string;
  token_enc: string | null;
  attempts: number;
  claimed_at: string;
}

interface ReminderKey {
  id: string;
  claimed_at: string;
}

function vaultKeyBytes(): Uint8Array {
  const hex = Deno.env.get("TELEGRAM_VAULT_KEY")?.trim() ?? "";
  if (!/^[0-9a-fA-F]{64}$/.test(hex)) {
    throw new Error("TELEGRAM_VAULT_KEY must be 64 hex chars (32 bytes)");
  }
  const out = new Uint8Array(32);
  for (let i = 0; i < 32; i++) out[i] = parseInt(hex.slice(i * 2, i * 2 + 2), 16);
  return out;
}

/** Decrypts a telegram_bots.token_enc blob with the shared vault key. */
async function decryptToken(tokenEncB64: string, keyRaw: Uint8Array): Promise<string> {
  const blob = Uint8Array.from(atob(tokenEncB64), (c) => c.charCodeAt(0));
  if (blob.length < 1 + NONCE_LEN + 16 || blob[0] !== VAULT_VERSION) {
    throw new Error(`unsupported vault blob (version ${blob[0]})`);
  }
  const nonce = blob.slice(1, 1 + NONCE_LEN);
  const ct = blob.slice(1 + NONCE_LEN);
  // Uint8Array<ArrayBufferLike> is not assignable to BufferSource under
  // TS6 lib types (the backing buffer may be SharedArrayBuffer); the
  // bytes came from atob so they are always a plain ArrayBuffer.
  const key = await crypto.subtle.importKey(
    "raw",
    keyRaw as unknown as BufferSource,
    { name: "AES-GCM" },
    false,
    ["decrypt"],
  );
  const pt = await crypto.subtle.decrypt({ name: "AES-GCM", iv: nonce }, key, ct);
  return new TextDecoder().decode(pt);
}

/** Telegram rate limit. The Go path sleeps retry_after once and retries once. */
class RateError extends Error {
  constructor(readonly retryAfter: number) {
    super(`telegram 429 retry_after=${retryAfter}`);
  }
}

async function sendMessage(token: string, chatId: string, text: string): Promise<void> {
  const res = await fetch(`${TELEGRAM_API}/bot${token}/sendMessage`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ chat_id: chatId, text }),
    signal: AbortSignal.timeout(SEND_TIMEOUT_MS),
  });
  if (res.status === 429) {
    const body = (await res.json().catch(() => ({}))) as {
      parameters?: { retry_after?: number };
    };
    throw new RateError(body.parameters?.retry_after ?? 5);
  }
  if (!res.ok) {
    // 400/403 are permanent for a bad chat or a bot that lost access.
    throw new Error(`telegram ${res.status}`);
  }
}

Deno.serve(async () => {
  const dsn = Deno.env.get("DELIVERY_DATABASE_URL");
  if (!dsn) {
    return new Response("DELIVERY_DATABASE_URL not set", { status: 500 });
  }

  let sql: ReturnType<typeof postgres>;
  let keyRaw: Uint8Array;
  try {
    sql = postgres(dsn, { max: 1, prepare: false });
    keyRaw = vaultKeyBytes();
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    return new Response(`config: ${msg}`, { status: 500 });
  }

  const complete = (id: string, claimedAt: string, status: "sent" | "dead") =>
    sql`select private.complete_reminder(${id}::uuid, ${claimedAt}::timestamptz, ${status}) as ok`;

  const fail = (id: string, claimedAt: string) =>
    sql`select private.fail_reminder(${id}::uuid, ${claimedAt}::timestamptz, ${MAX_ATTEMPTS}) as status`;

  const report = { claimed: 0, sent: 0, dead: 0, retried: 0, failed: 0 };

  try {
    // The claim functions RETURNS SETOF jsonb, so each row is a single
    // column `j` holding the payload.
    const rows = await sql<{ j: ClaimedReminder }[]>`
      select j from private.claim_due_reminders(${BATCH}) as j
    `;
    const batch = rows.map((r) =>
      (typeof r.j === "string" ? JSON.parse(r.j) : r.j) as ClaimedReminder
    );
    report.claimed = batch.length;

    for (const d of batch) {
      const ref: ReminderKey = { id: d.id, claimed_at: d.claimed_at };
      try {
        // Permanently undeliverable, same reasons as deliver():
        // no owning bot, or a token we cannot decrypt.
        if (!d.token_enc) {
          await complete(ref.id, ref.claimed_at, "dead");
          report.dead++;
          continue;
        }
        let token: string;
        try {
          token = await decryptToken(d.token_enc, keyRaw);
        } catch {
          console.error(JSON.stringify({ event: "reminder_undecryptable", id: d.id }));
          await complete(ref.id, ref.claimed_at, "dead");
          report.dead++;
          continue;
        }

        const chatId = Number(d.chat_id);
        if (!Number.isFinite(chatId) || chatId === 0) {
          await complete(ref.id, ref.claimed_at, "dead");
          report.dead++;
          continue;
        }

        const body = `⏰ Reminder: ${d.text}`;
        try {
          await sendMessage(token, String(chatId), body);
        } catch (err) {
          if (err instanceof RateError) {
            // Honour retry_after once, then give up on this tick.
            await new Promise((r) => setTimeout(r, Math.min(err.retryAfter, 30) * 1000));
            await sendMessage(token, String(chatId), body);
            report.retried++;
          } else {
            throw err;
          }
        }

        await complete(ref.id, ref.claimed_at, "sent");
        report.sent++;
      } catch (err) {
        console.error(JSON.stringify({
          event: "reminder_send_failed",
          id: d.id,
          err: err instanceof Error ? err.message : String(err),
        }));
        // Releases the claim and burns one attempt; the row is dead at
        // MAX_ATTEMPTS, so a permanently bad chat cannot poison the loop.
        await fail(ref.id, ref.claimed_at);
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
