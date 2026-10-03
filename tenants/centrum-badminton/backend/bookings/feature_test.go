// Mandatory isolation + CRUD + the double-booking concurrency test
// for the bookings feature. The double-booking test is the proof
// that the exclusion constraint works under real concurrency: two
// requests for the same facility+time can't both win.
//
// DONE: Phase 4 (sports vertical) / STANDARDS A, D, F
package bookings

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/auth"
	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/internal/features"
	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
	"github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/testutil"
)

type harness struct {
	router    http.Handler
	pool      *pgxpool.Pool // app role (RLS applies)
	adminPool *pgxpool.Pool // superuser (seeding bypasses RLS)
	rdb       *redis.Client
}

func setup(t *testing.T) *harness {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	rurl := os.Getenv("REDIS_URL")
	if dsn == "" || rurl == "" {
		t.Skip("DATABASE_URL / REDIS_URL not set — skipping integration test")
	}
	adminDSN := os.Getenv("DATABASE_ADMIN_URL")
	if adminDSN == "" {
		adminDSN = dsn
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	adminPool, err := db.NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adminPool.Close)
	bootstrapAppRole(t, ctx, adminPool)
	if err := db.Migrate(ctx, adminPool); err != nil {
		t.Fatal(err)
	}
	// Clean this test's tenants under an advisory lock so the four
	// sports packages don't race their DELETE+INSERT on the same
	// tables (SQLSTATE XX000 "tuple concurrently updated").
	if err := testutil.LockAndRun(ctx, adminPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM sports_bookings WHERE tenant_id IN ('bk-a','bk-b');
			 DELETE FROM sports_facilities WHERE tenant_id IN ('bk-a','bk-b');
			 DELETE FROM clients WHERE tenant_id IN ('bk-a','bk-b');`); err != nil {
			return err
		}
		seedClientTx(t, ctx, tx, "bk-a", "bk-a", "tok-bk-a", 1000000, "active", "records", "facilities", "bookings")
		seedClientTx(t, ctx, tx, "bk-b", "bk-b", "tok-bk-b", 1000000, "active", "records", "facilities", "bookings")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pool, err := db.NewPool(ctx, toAppDSN(dsn))
	if err != nil {
		t.Fatalf("connect as app: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := adminPool.Exec(ctx,
		`DELETE FROM sports_bookings WHERE tenant_id IN ('bk-a','bk-b');
		 DELETE FROM sports_facilities WHERE tenant_id IN ('bk-a','bk-b');`); err != nil {
		t.Fatal(err)
	}

	rdb, err := redisx.NewClient(ctx, rurl)
	if err != nil {
		t.Fatal(err)
	}
	rdb.FlushDB(ctx)
	t.Cleanup(func() { rdb.Close() })

	cli := &cache.Client{DB: pool, RDB: rdb}
	features.ResetForTest()
	Register()
	fr := features.BuildVerticalRouter(features.BuilderDeps{Pool: pool, Cache: cli}, "sports")
	return &harness{router: fr, pool: pool, adminPool: adminPool, rdb: rdb}
}

func bootstrapAppRole(t *testing.T, ctx context.Context, admin *pgxpool.Pool) {
	t.Helper()
	stmts := []string{
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='app') THEN
				CREATE ROLE app LOGIN;
			ELSE
				ALTER ROLE app LOGIN;
			END IF;
		END $$`,
		`GRANT USAGE ON SCHEMA public TO app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON clients TO app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON usage TO app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_records TO app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON sports_facilities TO app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON sports_bookings TO app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON sports_inventory TO app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON sports_members TO app`,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app`,
	}
	for _, s := range stmts {
		if _, err := admin.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
}

func toAppDSN(dsn string) string {
	i := strings.Index(dsn, "://")
	if i < 0 {
		return dsn
	}
	rest := dsn[i+3:]
	at := strings.Index(rest, "@")
	if at < 0 {
		return dsn
	}
	return dsn[:i+3] + "app" + rest[at:]
}

func seedClient(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name, tenant, token string, quota int, status string, feats ...string) {
	t.Helper()
	hash := auth.HashToken(token)
	if _, err := pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features)
		 VALUES ($1,$2,$3,$4,$5,'sports',$6)`,
		name, tenant, hash, quota, status, feats,
	); err != nil {
		t.Fatal(err)
	}
}

// seedClientTx is the tx variant used inside the locked setup.
func seedClientTx(t *testing.T, ctx context.Context, tx pgx.Tx, name, tenant, token string, quota int, status string, feats ...string) {
	t.Helper()
	hash := auth.HashToken(token)
	if _, err := tx.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features)
		 VALUES ($1,$2,$3,$4,$5,'sports',$6)`,
		name, tenant, hash, quota, status, feats,
	); err != nil {
		t.Fatal(err)
	}
}

func seedFacility(t *testing.T, ctx context.Context, admin *pgxpool.Pool, tenant string) string {
	t.Helper()
	var id string
	// Locked: seeding a facility INSERTS into sports_facilities while
	// other sports packages DELETE from it during setup — Postgres can
	// raise "tuple concurrently updated" (SQLSTATE XX000) at the page
	// level even for different tenants.
	err := testutil.LockAndRun(ctx, admin, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO sports_facilities (tenant_id, name, type, price_per_hour, open_time, close_time)
			 VALUES ($1,'Court 3','badminton',50000,'06:00','22:00') RETURNING id::text`,
			tenant,
		).Scan(&id)
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func doReq(t *testing.T, h http.Handler, method, path, token, body string) *http.Response {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	var req *http.Request
	if body != "" {
		req, _ = http.NewRequest(method, srv.URL+path, bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req, _ = http.NewRequest(method, srv.URL+path, nil)
	}
	if token != "" {
		req.Header.Set("X-Client-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b := make([]byte, 4096)
	n, _ := resp.Body.Read(b)
	return string(b[:n])
}

func TestBookings_CRUD(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fac := seedFacility(t, ctx, h.adminPool, "bk-a")

	// Create
	body := `{"facility_id":"` + fac + `","date":"2026-09-05","start_time":"19:00","end_time":"20:00","customer_name":"Alice","customer_phone":"+628123","price":50000}`
	resp := doReq(t, h.router, "POST", "/bookings", "tok-bk-a", body)
	if resp.StatusCode != 201 {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatal("create: no id")
	}
	// List by date
	resp2 := doReq(t, h.router, "GET", "/bookings?date=2026-09-05", "tok-bk-a", "")
	if resp2.StatusCode != 200 {
		t.Fatalf("list status=%d", resp2.StatusCode)
	}
	m2 := readJSON(t, resp2)
	items, _ := m2["items"].([]any)
	if len(items) != 1 {
		t.Errorf("items=%d want 1", len(items))
	}
	// Get one
	resp3 := doReq(t, h.router, "GET", "/bookings/"+id, "tok-bk-a", "")
	if resp3.StatusCode != 200 {
		t.Errorf("get status=%d", resp3.StatusCode)
	}
	// Patch → checked_in
	resp4 := doReq(t, h.router, "PATCH", "/bookings/"+id, "tok-bk-a", `{"status":"checked_in"}`)
	if resp4.StatusCode != 200 {
		t.Errorf("patch status=%d", resp4.StatusCode)
	}
	// Cancel from checked_in must be REFUSED. This state describes a slot
	// that was really played; cancelling it would erase the court event
	// and free an occupied slot. The DELETE used to have no status
	// condition at all, so it silently succeeded here.
	resp5 := doReq(t, h.router, "DELETE", "/bookings/"+id, "tok-bk-a", "")
	if resp5.StatusCode != 409 {
		t.Errorf("delete from checked_in status=%d, want 409; body=%s", resp5.StatusCode, readAll(t, resp5))
	}
	if !strings.Contains(readAll(t, resp5), "cancellable") {
		t.Errorf("409 body should explain the state is not cancellable, got %s", readAll(t, resp5))
	}

	// Cancel from a cancellable state must still work. Use a second
	// booking: `checked_in` can only move to completed/no_show, so the
	// first one cannot be walked back to a cancellable state.
	body2 := `{"facility_id":"` + fac + `","date":"2026-09-05","start_time":"21:00","end_time":"22:00","customer_name":"Alice","price":50000}`
	resp6 := doReq(t, h.router, "POST", "/bookings", "tok-bk-a", body2)
	if resp6.StatusCode != 201 {
		t.Fatalf("create second status=%d body=%s", resp6.StatusCode, readAll(t, resp6))
	}
	id2, _ := readJSON(t, resp6)["id"].(string)
	resp7 := doReq(t, h.router, "DELETE", "/bookings/"+id2, "tok-bk-a", "")
	if resp7.StatusCode != 200 {
		t.Errorf("delete from confirmed status=%d, want 200; body=%s", resp7.StatusCode, readAll(t, resp7))
	}
}

// TestBookings_CancelKeepsTheRow pins that DELETE is a SOFT cancel. A
// hard delete would cascade into sports_payments and destroy the ledger
// entry the reports read, so the row must survive.
func TestBookings_CancelKeepsTheRow(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fac := seedFacility(t, ctx, h.adminPool, "bk-a")

	body := `{"facility_id":"` + fac + `","date":"2026-09-06","start_time":"19:00","end_time":"20:00","customer_name":"Bob","price":50000}`
	resp := doReq(t, h.router, "POST", "/bookings", "tok-bk-a", body)
	if resp.StatusCode != 201 {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	id, _ := readJSON(t, resp)["id"].(string)

	if r := doReq(t, h.router, "DELETE", "/bookings/"+id, "tok-bk-a", ""); r.StatusCode != 200 {
		t.Fatalf("delete status=%d body=%s", r.StatusCode, readAll(t, r))
	}

	// Still readable, and its status is 'cancelled' — not gone.
	resp2 := doReq(t, h.router, "GET", "/bookings/"+id, "tok-bk-a", "")
	if resp2.StatusCode != 200 {
		t.Fatalf("booking vanished after cancel: status=%d", resp2.StatusCode)
	}
	if got, _ := readJSON(t, resp2)["status"].(string); got != "cancelled" {
		t.Errorf("status after cancel = %q, want \"cancelled\"", got)
	}

	// Cancelling twice is a conflict, not a silent success.
	if r := doReq(t, h.router, "DELETE", "/bookings/"+id, "tok-bk-a", ""); r.StatusCode != 409 {
		t.Errorf("second cancel status=%d, want 409", r.StatusCode)
	}

	// An unknown id is a 404, distinct from the 409 above.
	if r := doReq(t, h.router, "DELETE", "/bookings/00000000-0000-0000-0000-000000000000", "tok-bk-a", ""); r.StatusCode != 404 {
		t.Errorf("delete unknown status=%d, want 404", r.StatusCode)
	}
}

// TestBookings_BlockedSlot pins that a court block is enforced on the
// write path, not just drawn on the grid.
//
// A "blokir" used to be purely cosmetic: the availability board greyed the
// slot out and nothing stopped anyone booking it, by form, by direct URL,
// or by the chat agent (which INSERTs straight into the table). This
// covers the three cases that were open.
func TestBookings_BlockedSlot(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fac := seedFacility(t, ctx, h.adminPool, "bk-a")

	// A pricing rule, so create gets past the price step and reaches the
	// block check rather than stopping at ErrNoRule.
	if _, err := h.adminPool.Exec(ctx,
		`INSERT INTO sports_pricing_rules (tenant_id, valid_from, open_time, close_time, day_start, day_end, price_day, price_evening)
		 VALUES ('bk-a','2026-01-01','07:00','23:00','07:00','17:00',40000,50000)`); err != nil {
		t.Fatal(err)
	}

	book := func(date, start, end, name string) *http.Response {
		return doReq(t, h.router, "POST", "/bookings", "tok-bk-a",
			`{"facility_id":"`+fac+`","date":"`+date+`","start_time":"`+start+
				`","end_time":"`+end+`","customer_name":"`+name+`"}`)
	}

	// Block 19:00-21:00 on one date, and 10:00-12:00 across ALL courts on
	// another (facility_id NULL).
	if _, err := h.adminPool.Exec(ctx,
		`INSERT INTO sports_court_blocks (tenant_id, facility_id, block_date, start_time, end_time, reason)
		 VALUES ('bk-a',$1,'2026-09-07','19:00','21:00','maintenance')`, fac); err != nil {
		t.Fatal(err)
	}
	if _, err := h.adminPool.Exec(ctx,
		`INSERT INTO sports_court_blocks (tenant_id, facility_id, block_date, start_time, end_time, reason)
		 VALUES ('bk-a',NULL,'2026-09-08','10:00','12:00','private event')`); err != nil {
		t.Fatal(err)
	}

	// Refused, with the reason.
	for _, c := range []struct{ date, start, end, why string }{
		{"2026-09-07", "19:00", "20:00", "fully inside"},
		{"2026-09-07", "19:30", "20:30", "straddling the start"},
		{"2026-09-07", "20:00", "21:00", "straddling the end"},
		{"2026-09-08", "10:30", "11:30", "inside an all-courts block"},
	} {
		resp := book(c.date, c.start, c.end, "Blocked")
		if resp.StatusCode != 409 {
			t.Errorf("%s %s-%s (%s): status %d, want 409; body=%s",
				c.date, c.start, c.end, c.why, resp.StatusCode, readAll(t, resp))
		}
	}

	// Adjacent slots are fine: overlap is half-open, so a block ending at
	// 12:00 must not refuse a booking starting at 12:00. Getting this
	// wrong would make an evening's last slot unbookable.
	for _, c := range []struct{ date, start, end, why string }{
		{"2026-09-07", "18:00", "19:00", "ends as the block starts"},
		{"2026-09-07", "21:00", "22:00", "starts as the block ends"},
		{"2026-09-08", "12:00", "13:00", "after an all-courts block"},
		{"2026-09-08", "08:00", "09:00", "before an all-courts block"},
	} {
		resp := book(c.date, c.start, c.end, "Free")
		if resp.StatusCode != 201 {
			t.Errorf("%s %s-%s (%s): status %d, want 201; body=%s",
				c.date, c.start, c.end, c.why, resp.StatusCode, readAll(t, resp))
		}
	}

	// The database trigger, which is what protects the paths that bypass
	// this handler -- the agent's book_slot INSERTs directly.
	_, err := h.adminPool.Exec(ctx,
		`INSERT INTO sports_bookings (tenant_id, facility_id, date, start_time, end_time, customer_name, status, price)
		 VALUES ('bk-a',$1,'2026-09-07','19:30','20:30','RawInsert','confirmed',50000)`, fac)
	if err == nil {
		t.Error("a direct INSERT into a blocked slot should have been refused by the trigger")
	} else if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("trigger error should mention the block, got: %v", err)
	}

	// And a direct INSERT into a free slot still works, so the trigger is
	// not simply refusing everything.
	if _, err := h.adminPool.Exec(ctx,
		`INSERT INTO sports_bookings (tenant_id, facility_id, date, start_time, end_time, customer_name, status, price)
		 VALUES ('bk-a',$1,'2026-09-07','15:00','16:00','RawFree','confirmed',50000)`, fac); err != nil {
		t.Errorf("direct INSERT into a free slot was refused: %v", err)
	}
}

// TestBookings_DoubleBooking is THE architecture-proving test: two
// concurrent requests for the same facility + overlapping time →
// exactly one wins, the other gets 409.
func TestBookings_DoubleBooking(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fac := seedFacility(t, ctx, h.adminPool, "bk-a")

	body := func(name string) string {
		return `{"facility_id":"` + fac + `","date":"2026-09-06","start_time":"18:00","end_time":"19:00","customer_name":"` + name + `"}`
	}

	// Fire both at once (real HTTP concurrency through the whole
	// stack: auth → gate → handler → exclusion constraint).
	var wg sync.WaitGroup
	results := make([]int, 2)
	for i, name := range []string{"Alice", "Bob"} {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			srv := httptest.NewServer(h.router)
			defer srv.Close()
			req, _ := http.NewRequest("POST", srv.URL+"/bookings", bytes.NewReader([]byte(body(name))))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Client-Token", "tok-bk-a")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results[i] = -1
				return
			}
			results[i] = resp.StatusCode
			resp.Body.Close()
		}(i, name)
	}
	wg.Wait()

	// Sort results so we can compare regardless of which won.
	sorted := []int{results[0], results[1]}
	if sorted[0] > sorted[1] {
		sorted[0], sorted[1] = sorted[1], sorted[0]
	}
	if sorted[0] != 201 || sorted[1] != 409 {
		t.Fatalf("expected [201 409], got %v (raw %v)", sorted, results)
	}

	// And the DB has exactly one booking for that slot.
	var n int
	h.adminPool.QueryRow(ctx,
		`SELECT count(*) FROM sports_bookings WHERE tenant_id='bk-a' AND facility_id=$1::uuid AND date='2026-09-06'`,
		fac).Scan(&n)
	if n != 1 {
		t.Errorf("DB has %d bookings for slot, want 1", n)
	}
}

// TestBookings_TenantIsolation — the mandatory isolation test.
func TestBookings_TenantIsolation(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	facA := seedFacility(t, ctx, h.adminPool, "bk-a")
	facB := seedFacility(t, ctx, h.adminPool, "bk-b")

	// A books.
	resp := doReq(t, h.router, "POST", "/bookings", "tok-bk-a",
		`{"facility_id":"`+facA+`","date":"2026-09-07","start_time":"10:00","end_time":"11:00","customer_name":"A"}`)
	m := readJSON(t, resp)
	id, _ := m["id"].(string)

	// B cannot see A's booking.
	respL := doReq(t, h.router, "GET", "/bookings?date=2026-09-07", "tok-bk-b", "")
	mL := readJSON(t, respL)
	items, _ := mL["items"].([]any)
	if len(items) != 0 {
		t.Errorf("B saw %d bookings, want 0", len(items))
	}
	// B cannot get A's by id.
	respG := doReq(t, h.router, "GET", "/bookings/"+id, "tok-bk-b", "")
	if respG.StatusCode != 404 {
		t.Errorf("B get status=%d want 404", respG.StatusCode)
	}
	// B can book the SAME time on B's OWN facility (no cross-tenant interference).
	respB := doReq(t, h.router, "POST", "/bookings", "tok-bk-b",
		`{"facility_id":"`+facB+`","date":"2026-09-07","start_time":"10:00","end_time":"11:00","customer_name":"B"}`)
	if respB.StatusCode != 201 {
		t.Errorf("B own-facility booking status=%d want 201", respB.StatusCode)
	}
}

// TestBookings_Validation: outside facility hours → 400.
func TestBookings_Validation(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fac := seedFacility(t, ctx, h.adminPool, "bk-a")

	// Outside open hours (open 06:00, close 22:00).
	resp := doReq(t, h.router, "POST", "/bookings", "tok-bk-a",
		`{"facility_id":"`+fac+`","date":"2026-09-05","start_time":"23:00","end_time":"23:30","customer_name":"x"}`)
	if resp.StatusCode != 400 {
		t.Errorf("outside hours status=%d want 400 (body=%s)", resp.StatusCode, readAll(t, resp))
	}
	// end before start.
	resp2 := doReq(t, h.router, "POST", "/bookings", "tok-bk-a",
		`{"facility_id":"`+fac+`","date":"2026-09-05","start_time":"20:00","end_time":"19:00","customer_name":"x"}`)
	if resp2.StatusCode != 400 {
		t.Errorf("end<start status=%d want 400", resp2.StatusCode)
	}
	// unknown facility.
	resp3 := doReq(t, h.router, "POST", "/bookings", "tok-bk-a",
		`{"facility_id":"00000000-0000-0000-0000-000000000000","date":"2026-09-05","start_time":"10:00","end_time":"11:00","customer_name":"x"}`)
	if resp3.StatusCode != 404 {
		t.Errorf("unknown facility status=%d want 404", resp3.StatusCode)
	}
}
