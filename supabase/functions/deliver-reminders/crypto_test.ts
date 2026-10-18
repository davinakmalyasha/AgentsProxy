// Round-trip + tamper tests for encryptPush, against an independent
// decryptor written from RFC 8291 / RFC 8188 (it does NOT reuse
// crypto.ts helpers — the point is to catch a shared misunderstanding,
// which is exactly how the missing second HKDF extract shipped once:
// FCM accepted every send, browsers silently dropped the payloads).
//
// Run: deno test --allow-read supabase/functions/deliver-reminders/crypto_test.ts

import { encryptPush } from "./crypto.ts";

const enc = new TextEncoder();
const dec = new TextDecoder();

function concat(...parts: Uint8Array<ArrayBufferLike>[]): Uint8Array<ArrayBuffer> {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let off = 0;
  for (const p of parts) {
    out.set(p, off);
    off += p.length;
  }
  return out;
}

function b64u(bytes: Uint8Array<ArrayBufferLike>): string {
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
}

// RFC 5869 via raw HMAC (independent implementation for the test).
async function hmac(keyBytes: Uint8Array<ArrayBuffer>, msg: Uint8Array<ArrayBuffer>): Promise<Uint8Array<ArrayBuffer>> {
  const key = await crypto.subtle.importKey("raw", keyBytes, { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  return new Uint8Array(await crypto.subtle.sign("HMAC", key, msg));
}
async function extract(salt: Uint8Array<ArrayBuffer>, ikm: Uint8Array<ArrayBuffer>): Promise<Uint8Array<ArrayBuffer>> {
  return hmac(salt, ikm);
}
async function expand(prk: Uint8Array<ArrayBuffer>, info: Uint8Array<ArrayBuffer>, len: number): Promise<Uint8Array<ArrayBuffer>> {
  const key = await crypto.subtle.importKey("raw", prk, { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  const out = new Uint8Array(len);
  let prev = new Uint8Array(0);
  let off = 0;
  let i = 1;
  while (off < len) {
    prev = new Uint8Array(await crypto.subtle.sign("HMAC", key, concat(prev, info, new Uint8Array([i++]))));
    const take = Math.min(prev.length, len - off);
    out.set(prev.subarray(0, take), off);
    off += take;
  }
  return out;
}

// Decrypt one aes128gcm record per RFC 8188 §2 + RFC 8291 §3.4.
async function decryptRecord(
  record: Uint8Array<ArrayBuffer>,
  uaPriv: CryptoKey,
  uaPubRaw: Uint8Array<ArrayBuffer>,
  authSecret: Uint8Array<ArrayBuffer>,
): Promise<Uint8Array<ArrayBuffer>> {
  const salt = record.slice(0, 16);
  const idlen = record[20];
  const asPub = record.slice(21, 21 + idlen);
  const ct = record.slice(21 + idlen);
  if (asPub.length !== 65 || asPub[0] !== 0x04) throw new Error("bad keyid");

  const asKey = await crypto.subtle.importKey("raw", asPub, { name: "ECDH", namedCurve: "P-256" }, false, []);
  const ecdh = new Uint8Array(
    await crypto.subtle.deriveBits({ name: "ECDH", public: asKey }, uaPriv, 256),
  );
  const keyInfo = concat(enc.encode("WebPush: info\x00"), uaPubRaw, asPub);
  const ikm = await expand(await extract(authSecret, ecdh), keyInfo, 32);
  const prk = await extract(salt, ikm);
  const cek = await expand(prk, enc.encode("Content-Encoding: aes128gcm\x00"), 16);
  const nonce = await expand(prk, enc.encode("Content-Encoding: nonce\x00"), 12);

  const aes = await crypto.subtle.importKey("raw", cek, "AES-GCM", false, ["decrypt"]);
  const plain = new Uint8Array(
    await crypto.subtle.decrypt({ name: "AES-GCM", iv: nonce }, aes, ct),
  );
  let d = plain.length - 1;
  while (d >= 0 && plain[d] === 0) d--;
  if (plain[d] !== 0x02) throw new Error("missing last-record delimiter");
  return plain.slice(0, d);
}

async function freshKeys() {
  const ua = await crypto.subtle.generateKey({ name: "ECDH", namedCurve: "P-256" }, true, ["deriveBits"]);
  const uaPubRaw = new Uint8Array(await crypto.subtle.exportKey("raw", ua.publicKey));
  const authSecret = crypto.getRandomValues(new Uint8Array(16));
  return { ua, uaPubRaw, authSecret, keys: { p256dh: b64u(uaPubRaw), auth: b64u(authSecret) } };
}

function assertEqualBytes(a: Uint8Array<ArrayBufferLike>, b: Uint8Array<ArrayBufferLike>, msg: string) {
  if (a.length !== b.length || !a.every((v, i) => v === b[i])) throw new Error(msg);
}

Deno.test("encryptPush round-trips through an independent RFC decryptor", async () => {
  const { ua, uaPubRaw, authSecret, keys } = await freshKeys();
  const payload = enc.encode(JSON.stringify({ title: "Centrum", body: "⏰ Reminder: test" }));
  const record = new Uint8Array(await encryptPush(keys, payload));

  if (record[20] !== 65 || record[21] !== 0x04) throw new Error("record header layout wrong");
  const out = await decryptRecord(record, ua.privateKey, uaPubRaw, authSecret);
  assertEqualBytes(out, payload, "decrypted plaintext mismatch");
  if (dec.decode(out) !== JSON.stringify({ title: "Centrum", body: "⏰ Reminder: test" })) {
    throw new Error("text mismatch");
  }
});

Deno.test("tampered ciphertext fails authentication", async () => {
  const { ua, uaPubRaw, authSecret, keys } = await freshKeys();
  const record = new Uint8Array(await encryptPush(keys, enc.encode("hello")));
  record[record.length - 1] ^= 0x01;
  let failed = false;
  try {
    await decryptRecord(record, ua.privateKey, uaPubRaw, authSecret);
  } catch {
    failed = true;
  }
  if (!failed) throw new Error("tampered record decrypted; AES-GCM auth not enforced");
});
