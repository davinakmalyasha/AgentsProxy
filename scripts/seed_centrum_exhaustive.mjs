#!/usr/bin/env node
// Seed exhaustive 5-10 data per entity for Centrum (tenant centrum) via proxy
// Uses X-Session-Token (owner) — single source, RLS via app.tenant_id
// Run: node --env-file=.env scripts/seed_centrum_exhaustive.mjs
// Requires: PROXY_URL=http://localhost:8080, ADMIN creates owner if missing, or use existing owner@centrumbadminton.id / test123

const PROXY = process.env.PROXY_URL || "http://localhost:8080";
const TENANT = "centrum";
const OWNER_EMAIL = "owner@centrumbadminton.id";
const OWNER_PASS = "test123";
const PEGAWAI_EMAIL = "pegawai@centrum.id";
const PEGAWAI_PASS = "test123";

async function login(email, pass) {
  const r = await fetch(`${PROXY}/v1/auth/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ tenant_id: TENANT, email, password: pass }),
  });
  const j = await r.json();
  if (!r.ok) throw new Error(`login ${email} ${r.status} ${JSON.stringify(j)}`);
  return j.token;
}

async function api(path, token, opts = {}) {
  const headers = { "Content-Type": "application/json" };
  if (token) headers["X-Session-Token"] = token;
  const r = await fetch(`${PROXY}${path}`, {
    headers,
    ...opts,
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const text = await r.text();
  let j;
  try { j = JSON.parse(text); } catch { j = text; }
  return { status: r.status, body: j, text };
}

async function ensureFacilities(token) {
  console.log("\n== Facilities 8 ==");
  const { body } = await api(`/v1/sports/facilities?tenant_id=${TENANT}`, token);
  const existing = (body.items || []).map(f => f.name);
  const want = [
    { name: "Lapangan 1", type: "badminton", capacity: 4, price_per_hour: 40000, open_time: "07:00", close_time: "23:00", sort_order: 1 },
    { name: "Lapangan 2", type: "badminton", capacity: 4, price_per_hour: 40000, open_time: "07:00", close_time: "23:00", sort_order: 2 },
    { name: "Lapangan 3", type: "badminton", capacity: 4, price_per_hour: 45000, open_time: "06:00", close_time: "23:00", sort_order: 3 },
    { name: "Soccer Mini", type: "soccer", capacity: 10, price_per_hour: 100000, open_time: "06:00", close_time: "23:00", sort_order: 4 },
    { name: "Kolam Renang", type: "swimming", capacity: 20, price_per_hour: 25000, open_time: "06:00", close_time: "18:00", sort_order: 5 },
    { name: "Padel Court", type: "padel", capacity: 4, price_per_hour: 120000, open_time: "07:00", close_time: "22:00", sort_order: 6 },
    { name: "Lapangan Paused", type: "badminton", capacity: 4, price_per_hour: 40000, open_time: "07:00", close_time: "23:00", sort_order: 7, status: "paused" },
    { name: "Lapangan VIP", type: "other", capacity: 6, price_per_hour: 60000, open_time: "08:00", close_time: "22:00", sort_order: 8 },
  ];
  for (const f of want) {
    if (existing.includes(f.name)) {
      console.log(` skip ${f.name} exists`);
      continue;
    }
    const { status, body: b } = await api(`/v1/sports/facilities`, token, { method: "POST", body: f });
    console.log(` create ${f.name}: ${status} ${JSON.stringify(b).slice(0,120)}`);
  }
  const { body: after } = await api(`/v1/sports/facilities?tenant_id=${TENANT}`, token);
  console.log(` facilities total: ${(after.items||[]).length}`);
  return after.items || [];
}

async function ensurePricing(token) {
  console.log("\n== Pricing 2 ==");
  const pricing = [
    { valid_from: "2026-01-01", open_time: "07:00", close_time: "23:00", day_start: "07:00", day_end: "17:00", price_day: 40000, price_evening: 50000, is_active: true },
    { valid_from: "2026-06-01", open_time: "07:00", close_time: "23:00", day_start: "07:00", day_end: "17:00", price_day: 45000, price_evening: 60000, is_active: true },
  ];
  for (const p of pricing) {
    const { status, body } = await api(`/v1/sports/pricing`, token, { method: "POST", body: p });
    console.log(` pricing ${p.valid_from}: ${status} ${JSON.stringify(body).slice(0,100)}`);
  }
}

async function ensureCustomers(token) {
  console.log("\n== Customers 8 ==");
  const want = [
    { name: "Budi Santoso", phone: "081111000001", notes: "vip" },
    { name: "Siti Aminah", phone: "081111000002" },
    { name: "Joko", phone: "081111000003", notes: "no_show 2" },
    { name: "Rina", phone: "081111000004" },
    { name: "Agus", phone: "081111000005" },
    { name: "Dewi", phone: "081111000006" },
    { name: "Eko", phone: "081111000007" },
    { name: "Lina", phone: "081111000008" },
  ];
  for (const c of want) {
    const { status, body } = await api(`/v1/sports/customers`, token, { method: "POST", body: { name: c.name, phone: c.phone, notes: c.notes || "" } });
    console.log(` customer ${c.name}: ${status} ${JSON.stringify(body).slice(0,80)}`);
  }
}

async function ensureBookings(facilities, token) {
  console.log("\n== Bookings 12 (all statuses) ==");
  const fac1 = facilities.find(f => f.name === "Lapangan 1")?.id || facilities[0]?.id;
  const fac2 = facilities.find(f => f.name === "Lapangan 2")?.id || facilities[1]?.id;
  if (!fac1) { console.log(" no facilities"); return []; }
  const today = new Date().toISOString().slice(0,10);
  const tomorrow = new Date(Date.now()+86400000).toISOString().slice(0,10);
  const bookings = [
    // 2 per status
    { facility_id: fac1, date: today, start_time: "07:00", end_time: "08:00", customer_name: "Budi Santoso", customer_phone: "081111000001", status: "pending", source: "whatsapp", price: 0 },
    { facility_id: fac1, date: today, start_time: "08:00", end_time: "09:00", customer_name: "Siti Aminah", customer_phone: "081111000002", status: "pending", source: "walk_in" },
    { facility_id: fac1, date: today, start_time: "09:00", end_time: "10:00", customer_name: "Joko", customer_phone: "081111000003", status: "confirmed", source: "whatsapp" },
    { facility_id: fac1, date: today, start_time: "10:00", end_time: "11:00", customer_name: "Rina", customer_phone: "081111000004", status: "confirmed", source: "whatsapp" },
    { facility_id: fac2, date: today, start_time: "09:00", end_time: "10:00", customer_name: "Agus", customer_phone: "081111000005", status: "checked_in" },
    { facility_id: fac2, date: today, start_time: "10:00", end_time: "11:00", customer_name: "Dewi", customer_phone: "081111000006", status: "checked_in" },
    { facility_id: fac1, date: tomorrow, start_time: "07:00", end_time: "08:00", customer_name: "Eko", customer_phone: "081111000007", status: "completed", price: 40000 },
    { facility_id: fac2, date: tomorrow, start_time: "08:00", end_time: "09:00", customer_name: "Lina", customer_phone: "081111000008", status: "completed" },
    { facility_id: fac1, date: tomorrow, start_time: "09:00", end_time: "10:00", customer_name: "Budi Santoso", customer_phone: "081111000001", status: "cancelled" },
    { facility_id: fac1, date: tomorrow, start_time: "10:00", end_time: "11:00", customer_name: "Joko", customer_phone: "081111000003", status: "no_show" },
    // override_price + deposit
    { facility_id: fac2, date: today, start_time: "11:00", end_time: "12:00", customer_name: "VIP Customer", customer_phone: "081111000009", status: "confirmed", price: 100000, deposit: 50000, notes: "override" },
    // re-bookable slot: cancelled slot should be re-bookable
    { facility_id: fac1, date: tomorrow, start_time: "09:00", end_time: "10:00", customer_name: "Rebook Test", customer_phone: "081111000010", status: "confirmed" },
  ];
  const ids = [];
  for (const b of bookings) {
    const { status, body } = await api(`/v1/sports/bookings`, token, { method: "POST", body: b });
    console.log(` booking ${b.date} ${b.start_time}-${b.end_time} ${b.status}: ${status} ${JSON.stringify(body).slice(0,80)}`);
    if (status === 201 && body.id) ids.push({ id: body.id, ...b });
    if (status === 409) console.log(`  -> 409 expected for re-book of cancelled? try next`);
  }
  // Patch some to no_show to test customer no_show_count
  const noShow = ids.find(b => b.customer_name === "Joko" && b.status === "confirmed");
  if (noShow) {
    const { status } = await api(`/v1/sports/bookings/${noShow.id}`, token, { method: "PATCH", body: { status: "no_show" } });
    console.log(` patch no_show ${noShow.id}: ${status}`);
  }
  return ids;
}

async function ensurePayments(bookings, token) {
  console.log("\n== Payments 10 (partial/paid/overpay cash/qris) ==");
  const confirmed = bookings.filter(b => b.status === "confirmed" && b.price !== undefined);
  const target = bookings.find(b => b.status === "confirmed") || bookings[0];
  if (!target) { console.log(" no bookings for payments"); return; }
  // Need booking with price; fetch it
  const { body } = await api(`/v1/sports/bookings?status=confirmed&tenant_id=${TENANT}`, token);
  const list = body.items || [];
  const b = list.find(x => x.customer_name === "Rina" || x.customer_name === "Joko") || list[0];
  if (!b) { console.log(" no confirmed booking found"); return; }
  console.log(` target booking ${b.id} price ${b.price} customer ${b.customer_name}`);
  const pays = [
    { booking_id: b.id, amount: 20000, method: "cash", note: "DP" },
    { booking_id: b.id, amount: 20000, method: "qris" },
    { booking_id: b.id, amount: 10000, method: "cash" }, // overpay test if price 40000
  ];
  for (const p of pays) {
    const { status, body } = await api(`/v1/sports/payments`, token, { method: "POST", body: p });
    console.log(` payment ${p.amount} ${p.method}: ${status} ${JSON.stringify(body).slice(0,80)}`);
  }
  // Additional booking payments
  const other = list.find(x => x.id !== b.id);
  if (other) {
    const { status } = await api(`/v1/sports/payments`, token, { method: "POST", body: { booking_id: other.id, amount: 40000, method: "cash" } });
    console.log(` payment other booking paid: ${status}`);
  }
}

async function ensureExpenses(token) {
  console.log("\n== Expenses 6 ==");
  const cats = [
    { category: "listrik", amount: 500000, spent_at: new Date().toISOString().slice(0,10), note: "PLN" },
    { category: "air", amount: 200000, spent_at: new Date().toISOString().slice(0,10) },
    { category: "gaji", amount: 1500000, spent_at: new Date().toISOString().slice(0,10), note: "pegawai" },
    { category: "shuttlecock", amount: 300000, spent_at: new Date(Date.now()-86400000).toISOString().slice(0,10) },
    { category: "lainnya", amount: 100000, spent_at: new Date(Date.now()-2*86400000).toISOString().slice(0,10) },
    { category: "sewa", amount: 2000000, spent_at: new Date().toISOString().slice(0,10).slice(0,7)+"-01" },
  ];
  for (const e of cats) {
    const { status } = await api(`/v1/sports/expenses`, token, { method: "POST", body: e });
    console.log(` expense ${e.category}: ${status}`);
  }
}

async function ensureBlocks(facilities, token) {
  console.log("\n== Court Blocks 4 ==");
  const fac1 = facilities[0]?.id;
  const today = new Date().toISOString().slice(0,10);
  const tomorrow = new Date(Date.now()+86400000).toISOString().slice(0,10);
  const blocks = [
    { block_date: today, start_time: "12:00", end_time: "13:00", reason: "maintenance", facility_id: fac1 },
    { block_date: tomorrow, start_time: "13:00", end_time: "15:00", reason: "private", facility_id: null },
    { block_date: tomorrow, start_time: "15:00", end_time: "16:00", reason: "cleaning", facility_id: fac1 },
    { block_date: today, start_time: "23:00", end_time: "23:00", reason: "test" }, // edge
  ];
  for (const b of blocks) {
    const body = { ...b };
    if (body.facility_id === null) delete body.facility_id;
    const { status } = await api(`/v1/sports/courtblocks`, token, { method: "POST", body });
    console.log(` block ${b.block_date} ${b.start_time}: ${status}`);
  }
}

async function ensureSeries(facilities, token) {
  console.log("\n== Series 3 ==");
  const fac1 = facilities[0]?.id;
  const series = [
    { facility_id: fac1, weekday: 1, start_time: "19:00", end_time: "21:00", start_date: new Date().toISOString().slice(0,10), customer_name: "Komunitas Senin", customer_phone: "081122233344", status: "active" },
    { facility_id: fac1, weekday: 3, start_time: "19:00", end_time: "21:00", start_date: new Date().toISOString().slice(0,10), end_date: new Date(Date.now()+30*86400000).toISOString().slice(0,10), customer_name: "Komunitas Rabu", status: "active" },
    { facility_id: fac1, weekday: 5, start_time: "20:00", end_time: "22:00", start_date: new Date().toISOString().slice(0,10), customer_name: "Komunitas Jumat", status: "paused" },
  ];
  for (const s of series) {
    const { status, body } = await api(`/v1/sports/series`, token, { method: "POST", body: s });
    console.log(` series w${s.weekday}: ${status} ${JSON.stringify(body).slice(0,60)}`);
  }
}

async function ensureKantin(token) {
  console.log("\n== Kantin 10 products + 5 sales ==");
  const products = [
    { name: "Aqua 600ml", category: "minuman", price: 5000, cost_price: 3000, track_stock: true, stock: 20, low_stock_threshold: 5, sort_order: 1 },
    { name: "Es Teh", category: "minuman", price: 5000, cost_price: 2000, track_stock: true, stock: 15, low_stock_threshold: 3, sort_order: 2 },
    { name: "Indomie Goreng", category: "makanan", price: 8000, cost_price: 5000, track_stock: true, stock: 10, low_stock_threshold: 2, sort_order: 3 },
    { name: "Kopi Hitam", category: "minuman", price: 6000, cost_price: 3000, track_stock: true, stock: 2, low_stock_threshold: 5, sort_order: 4 },
    { name: "Roti Bakar", category: "makanan", price: 10000, cost_price: 6000, track_stock: true, stock: 8, low_stock_threshold: 2, sort_order: 5 },
    { name: "Shuttlecock", category: "lainnya", price: 120000, cost_price: 100000, track_stock: true, stock: 5, low_stock_threshold: 2, sort_order: 6 },
    { name: "Kaos Kaki", category: "lainnya", price: 25000, cost_price: 15000, track_stock: false, stock: 0, low_stock_threshold: 0, sort_order: 7 },
    { name: "Pocari", category: "minuman", price: 7000, cost_price: 4000, track_stock: true, stock: 12, low_stock_threshold: 3, sort_order: 8 },
    { name: "Gorengan", category: "makanan", price: 2000, cost_price: 1000, track_stock: true, stock: 20, low_stock_threshold: 5, sort_order: 9 },
    { name: "Susu Kotak", category: "minuman", price: 6000, cost_price: 3500, track_stock: true, stock: 0, low_stock_threshold: 2, sort_order: 10, is_active: false },
  ];
  const prodIds = [];
  for (const p of products) {
    const { status, body } = await api(`/v1/sports/kantin/products`, token, { method: "POST", body: p });
    console.log(` product ${p.name}: ${status}`);
    if (status === 201 && body.id) prodIds.push({ ...p, id: body.id });
  }
  // fetch to get ids if already exists
  if (prodIds.length < 5) {
    const { body } = await api(`/v1/sports/kantin/products?tenant_id=${TENANT}`, token);
    for (const it of (body.items||[])) {
      if (!prodIds.find(x=>x.name===it.name)) prodIds.push(it);
    }
  }
  console.log(` products total: ${prodIds.length}`);
  // 5 sales
  for (let i=0;i<5;i++) {
    const items = [
      { product_id: prodIds[0]?.id, qty: 2 },
      { product_id: prodIds[2]?.id, qty: 1 },
    ].filter(x=>x.product_id);
    if (items.length===0) break;
    const { status, body } = await api(`/v1/sports/kantin/sales`, token, { method: "POST", body: { items, customer_name: `Cust ${i+1}`, method: i%2===0?"cash":"qris", note: `sale ${i+1}` } });
    console.log(` sale ${i+1}: ${status} ${JSON.stringify(body).slice(0,80)}`);
  }
}

async function main(){
  const token = await login(OWNER_EMAIL, OWNER_PASS);
  console.log(`owner token ok ${token.slice(0,8)}...`);
  const facilities = await ensureFacilities(token);
  await ensurePricing(token);
  await ensureCustomers(token);
  const bookings = await ensureBookings(facilities, token);
  await ensurePayments(bookings, token);
  await ensureExpenses(token);
  await ensureBlocks(facilities, token);
  await ensureSeries(facilities, token);
  await ensureKantin(token);
  console.log("\n== DONE exhaustive seed ==");
  // Verify counts
  const checks = [
    "/v1/sports/facilities",
    "/v1/sports/pricing",
    "/v1/sports/customers",
    "/v1/sports/courtblocks",
    "/v1/sports/series",
    "/v1/sports/kantin/products",
    "/v1/sports/expenses",
  ];
  for (const p of checks) {
    const { body } = await api(`${p}?tenant_id=${TENANT}`, token);
    console.log(`${p}: ${(body.items||body||[]).length || JSON.stringify(body).slice(0,60)} items`);
  }
}
main().catch(e=>{ console.error(e); process.exit(1); });
