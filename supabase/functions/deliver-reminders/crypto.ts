// RFC8291 aes128gcm Web Push encryption + VAPID signing (WebCrypto).
//
// Interop: live-verified 2026-10-18 — a real Chrome subscription
// received and displayed a production reminder end to end. That test
// caught what the earlier Deno round-trip could not: the encoder was
// missing RFC 8188's second HKDF extract (PRK = Extract(record_salt,
// IKM)); FCM accepted the opaque body (it never decrypts) and every
// browser dropped it silently. Both halves are pinned by crypto_test.ts
// against an RFC-written decryptor — not a shared helper, so a shared
// misunderstanding fails the test. The Go PushSender (internal/reminder,
// webpush-go) is the long-running-host driver; this Edge driver serves
// the pg_cron ticks (ops/schedule-reminders.sql). Both share the same
// claim protocol.

const RS = 4096;

function concat(...parts: Uint8Array<ArrayBufferLike>[]): Uint8Array<ArrayBuffer> {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let off = 0;
  for (const p of parts) {
    out.set(p, off);
    off += p.length;
  }
  return out;
}

function b64url(bytes: Uint8Array<ArrayBufferLike>): string {
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
}

function unb64url(s: string): Uint8Array<ArrayBuffer> {
  const bin = atob(s.replaceAll("-", "+").replaceAll("_", "/"));
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

// RFC 5869 HKDF via raw HMAC. WebCrypto's "HKDF" deriveBits always does
// extract+expand together, which cannot express the RFC 8291/8188 chain
// (extract → expand → extract → expand). Building both halves from
// HMAC-SHA-256 is exact:
//   Extract(salt, ikm) = HMAC(key=salt, msg=ikm)
//   Expand(prk, info, L) = T(1) || T(2) || ... ; T(i) = HMAC(key=prk, T(i-1) || info || i)
async function hmac(keyBytes: Uint8Array<ArrayBuffer>, msg: Uint8Array<ArrayBuffer>): Promise<Uint8Array<ArrayBuffer>> {
	const key = await crypto.subtle.importKey("raw", keyBytes, { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
	return new Uint8Array(await crypto.subtle.sign("HMAC", key, msg));
}

async function hkdfExtract(salt: Uint8Array<ArrayBuffer>, ikm: Uint8Array<ArrayBuffer>): Promise<Uint8Array<ArrayBuffer>> {
	return hmac(salt, ikm);
}

async function hkdfExpand(prk: Uint8Array<ArrayBuffer>, info: Uint8Array<ArrayBuffer>, len: number): Promise<Uint8Array<ArrayBuffer>> {
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

export interface PushKeys {
  p256dh: string;
  auth: string;
}

// encryptPush returns a full aes128gcm record for one subscription.
export async function encryptPush(keys: PushKeys, plaintext: Uint8Array<ArrayBuffer>): Promise<Uint8Array<ArrayBuffer>> {
  const uaPub = unb64url(keys.p256dh);
  const authSecret = unb64url(keys.auth);
  if (uaPub.length !== 65 || uaPub[0] !== 0x04) throw new Error("bad p256dh");
  if (authSecret.length !== 16) throw new Error("bad auth secret");

  const salt = crypto.getRandomValues(new Uint8Array(16));
  const asKey = await crypto.subtle.generateKey({ name: "ECDH", namedCurve: "P-256" }, true, [
    "deriveBits",
  ]);
	const asPubRaw = new Uint8Array(await crypto.subtle.exportKey("raw", asKey.publicKey) as ArrayBuffer);
	const uaPubKey = await crypto.subtle.importKey(
		"raw",
		uaPub,
    { name: "ECDH", namedCurve: "P-256" },
    false,
    [],
  );
  const ecdhBits = await crypto.subtle.deriveBits(
    { name: "ECDH", public: uaPubKey },
    asKey.privateKey,
    256,
  );
  const ecdh = new Uint8Array(ecdhBits);
  const enc = new TextEncoder();
  const keyInfo = concat(enc.encode("WebPush: info\x00"), uaPub, asPubRaw);
  // RFC 8291 §3.4: PRK_key = Extract(auth_secret, ecdh); IKM = Expand(PRK_key, key_info, 32).
  const ikm = await hkdfExpand(await hkdfExtract(authSecret, ecdh), keyInfo, 32);
  // RFC 8188 §2.1: PRK = Extract(record_salt, IKM) — the second extract
  // is load-bearing: expanding straight from IKM produces a payload the
  // browser cannot decrypt, and FCM accepts it anyway (it never decrypts),
  // so the failure is silent until a device test.
  const prk = await hkdfExtract(salt, ikm);
  const cek = await hkdfExpand(prk, enc.encode("Content-Encoding: aes128gcm\x00"), 16);
  const nonce = await hkdfExpand(prk, enc.encode("Content-Encoding: nonce\x00"), 12);

  const aesKey = await crypto.subtle.importKey("raw", cek as BufferSource, "AES-GCM", false, [
    "encrypt",
  ]);
  const padded = concat(plaintext, new Uint8Array([0x02]));
  const ct = new Uint8Array(
	await crypto.subtle.encrypt({ name: "AES-GCM", iv: nonce }, aesKey, padded),
  );

  const rs = new Uint8Array(4);
  new DataView(rs.buffer).setUint32(0, RS);
  return concat(salt, rs, new Uint8Array([asPubRaw.length]), asPubRaw, ct);
}

// vapidAuth builds the ES256 VAPID Authorization value (raw R||S).
// Keys are the standard base64url forms (same values as the Go side's
// VAPID_PUBLIC_KEY/VAPID_PRIVATE_KEY env): raw 32-byte private + 65-byte
// uncompressed public. The public halves become the JWK x/y.
export async function vapidAuth(
  publicKey: string,
  privateKey: string,
  subscriber: string,
  aud: string,
): Promise<string> {
  const enc = new TextEncoder();
  const header = b64url(enc.encode(JSON.stringify({ typ: "JWT", alg: "ES256" })));
  const exp = Math.floor(Date.now() / 1000) + 12 * 3600;
  const payload = b64url(enc.encode(JSON.stringify({ aud, exp, sub: subscriber })));
  const tb = `${header}.${payload}`;
  const priv = unb64url(privateKey);
  const pub = unb64url(publicKey);
  if (priv.length !== 32 || pub.length !== 65 || pub[0] !== 0x04) throw new Error("bad VAPID keys");
  const key = await crypto.subtle.importKey("jwk", {
    kty: "EC",
    crv: "P-256",
    x: b64url(pub.slice(1, 33)),
    y: b64url(pub.slice(33, 65)),
    d: b64url(priv),
    ext: true,
  }, { name: "ECDSA", namedCurve: "P-256" }, false, ["sign"]);
  const rawSig = new Uint8Array(
    await crypto.subtle.sign({ name: "ECDSA", hash: "SHA-256" }, key, enc.encode(tb)),
  );
  // Deno returns ECDSA signatures as raw R||S (IEEE P1363, 64 bytes),
  // NOT ASN.1 DER — JOSE wants exactly this, so no conversion. (Node
  // returns DER; this code runs only on Deno/Edge. If it ever moves
  // runtimes, re-verify with the round-trip test.)
  if (rawSig.length !== 64) throw new Error("unexpected signature length");
  return `${tb}.${b64url(rawSig)}`;
}
