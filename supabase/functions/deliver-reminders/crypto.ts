// RFC8291 aes128gcm Web Push encryption + VAPID signing (WebCrypto).
//
// Interop verified 2026-10-15 by Deno round-trip (encrypt → decrypt
// with known keys → plaintext; VAPID JWT verified against the public
// key), not against a live push service. The Go PushSender
// (internal/reminder, webpush-go) is the long-running-host driver; this
// Edge driver serves the pg_cron ticks (ops/schedule-reminders.sql,
// installed on prod 2026-10-15). Both share the same claim protocol.

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

async function hkdfExtract(salt: Uint8Array<ArrayBuffer>, ikm: Uint8Array<ArrayBuffer>): Promise<Uint8Array<ArrayBuffer>> {
	const key = await crypto.subtle.importKey("raw", ikm, "HKDF", false, ["deriveBits"]);
	const bits = await crypto.subtle.deriveBits(
		{ name: "HKDF", hash: "SHA-256", salt: salt, info: new Uint8Array(0) },
		key,
		256,
	);
	return new Uint8Array(bits);
}

async function hkdfExpand(prk: Uint8Array<ArrayBuffer>, info: Uint8Array<ArrayBuffer>, len: number): Promise<Uint8Array<ArrayBuffer>> {
	const key = await crypto.subtle.importKey("raw", prk, "HKDF", false, ["deriveBits"]);
	const bits = await crypto.subtle.deriveBits(
		{ name: "HKDF", hash: "SHA-256", salt: new Uint8Array(0), info: info },
		key,
		len * 8,
	);
	return new Uint8Array(bits);
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
  const prk = await hkdfExpand(await hkdfExtract(authSecret, ecdh), keyInfo, 32);
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
