#!/usr/bin/env node
// Provision a tenant through the admin API. Replaces migration 003,
// which used to insert a published `dev-token` credential on EVERY
// environment including production (Phase 1.1 retired it).
//
// No migration seeds tenants any more, so a fresh database has an EMPTY
// clients table and every public read answers 400 "unknown tenant" until
// one is created. This is the supported way to create it.
//
// Which tenant: the proxy hard-codes "centrum" as its fallback tenant in
// four places (records/identity.go, api/agent_chat.go), and the Centrum
// Badminton frontend sends tenant_id=centrum, so that is the default
// here. Override with TENANT_ID / TENANT_NAME / TENANT_VERTICAL.
//
// vertical matters: admin.Create defaults it to "fnb" and derives the
// feature set from it, so omitting it silently provisions a food-and-beverage
// tenant. Pass the real one (sports / clinic / gym).
//
// Run: node --env-file=.env scripts/seed-dev.mjs
// Requires: PROXY_URL (default http://localhost:8080), ADMIN_API_KEY in .env

const PROXY = process.env.PROXY_URL || "http://localhost:8080";
const ADMIN_KEY = process.env.ADMIN_API_KEY;
if (!ADMIN_KEY) {
  console.error("ADMIN_API_KEY not set in .env — refusing to run.");
  process.exit(1);
}

const TENANT = process.env.TENANT_ID || "centrum";
const NAME = process.env.TENANT_NAME || TENANT;
const VERTICAL = process.env.TENANT_VERTICAL || "sports";
const QUOTA = Number(process.env.TENANT_QUOTA || 500000);
// New projects get their own schema; omit this for the legacy centrum row.
const SCHEMA = process.env.TENANT_SCHEMA || "";

async function main() {
  const r = await fetch(`${PROXY}/v1/admin/clients`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "Authorization": `Bearer ${ADMIN_KEY}`,
    },
    body: JSON.stringify({
      name: NAME,
      tenant_id: TENANT,
      vertical: VERTICAL,
      monthly_quota_tokens: QUOTA,
      ...(SCHEMA ? { schema: SCHEMA } : {}),
    }),
  });
  const text = await r.text();
  let body;
  try { body = JSON.parse(text); } catch { body = text; }
  if (!r.ok) {
    // Idempotent: a tenant that already exists is not an error.
    // The admin API returns 409 on duplicate tenant_id.
    if (r.status === 409) {
      console.log(`tenant "${TENANT}" already exists — nothing to do.`);
      console.log("Its client token is only returned at creation time;");
      console.log("if you lost it, delete the row and re-run this script.");
      return;
    }
    console.error(`seed ${TENANT} failed: ${r.status} ${JSON.stringify(body)}`);
    if (r.status === 500) {
      console.error(
        "A bare 500 here usually means the admin route is running on the\n" +
        "read-only app pool: app_runtime has only SELECT on clients."
      );
    }
    process.exit(1);
  }
  console.log(`tenant "${TENANT}" ready (vertical: ${VERTICAL}):`);
  console.log(JSON.stringify(body, null, 2));
  console.log("\nSave the client token above for local testing (X-Client-Token).");
  console.log("Then verify: curl -s \"$PROXY_URL/v1/sports/availability/?tenant_id=" + TENANT + "&type=" + VERTICAL + "\"");
}

main().catch((e) => { console.error(e.message); process.exit(1); });
