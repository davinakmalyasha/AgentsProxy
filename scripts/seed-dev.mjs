#!/usr/bin/env node
// Seed a local dev client via the admin API. Replaces migration 003,
// which used to insert a published `dev-token` credential on EVERY
// environment including production (Phase 1.1 retired it).
//
// This script is dev-only: it runs against your local proxy, creates a
// `dev` tenant with a fresh random token, and prints the token for your
// local .env / testing. Nothing here runs in production.
//
// Run: node --env-file=.env scripts/seed-dev.mjs
// Requires: PROXY_URL (default http://localhost:8080), ADMIN_API_KEY in .env

const PROXY = process.env.PROXY_URL || "http://localhost:8080";
const ADMIN_KEY = process.env.ADMIN_API_KEY;
if (!ADMIN_KEY) {
  console.error("ADMIN_API_KEY not set in .env — refusing to run.");
  process.exit(1);
}

async function main() {
  const r = await fetch(`${PROXY}/v1/admin/clients`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "Authorization": `Bearer ${ADMIN_KEY}`,
    },
    body: JSON.stringify({
      name: "dev",
      tenant_id: "dev",
      monthly_quota_tokens: 1000000,
    }),
  });
  const text = await r.text();
  let body;
  try { body = JSON.parse(text); } catch { body = text; }
  if (!r.ok) {
    // Idempotent: a `dev` tenant that already exists is not an error.
    // The admin API returns 409 on duplicate tenant_id.
    if (r.status === 409) {
      console.log("dev tenant already exists — nothing to do.");
      console.log("If you need its token, check the clients table or re-create it.");
      return;
    }
    throw new Error(`seed dev ${r.status} ${JSON.stringify(body)}`);
  }
  console.log("dev tenant ready:");
  console.log(JSON.stringify(body, null, 2));
  console.log("\nSave the client token above for local testing (X-Client-Token).");
}

main().catch((e) => { console.error(e.message); process.exit(1); });
