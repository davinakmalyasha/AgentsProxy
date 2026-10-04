// Payments feature tests: CRUD + the DB-trigger payment_status recalc
// (unpaid → partial → paid) + isolation.
//
// DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F
package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
	adminPool *pgxpool.Pool
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
	if err := testutil.LockAndRun(ctx, adminPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM sports_payments WHERE tenant_id IN ('pay-a','pay-b');
			 DELETE FROM sports_bookings WHERE tenant_id IN ('pay-a','pay-b');
			 DELETE FROM sports_facilities WHERE tenant_id IN ('pay-a','pay-b');
			 DELETE FROM clients WHERE tenant_id IN ('pay-a','pay-b');`); err != nil {
			return err
		}
		for _, c := range [][]string{
			{"pay-a", "pay-a", "tok-pay-a"},
			{"pay-b", "pay-b", "tok-pay-b"},
		} {
			if err := seedClientTx(t, ctx, tx, c[0], c[1], c[2], 1000000, "active", "records", "facilities", "bookings", "payments"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pool, err := db.NewPool(ctx, toAppDSN(dsn))
	if err != nil {
		t.Fatalf("connect as app: %v", err)
	}
	t.Cleanup(pool.Close)

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
	return &harness{router: fr, adminPool: adminPool, rdb: rdb}
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
		`GRANT SELECT, INSERT, UPDATE, DELETE ON sports_payments TO app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON sports_pricing_rules TO app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON sports_expenses TO app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON sports_customers TO app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON sports_court_blocks TO app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON sports_recurring_series TO app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON sports_kantin_products TO app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON sports_kantin_sales TO app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON sports_kantin_sale_items TO app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON sports_kantin_stock_movements TO app`,
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

func seedClientTx(t *testing.T, ctx context.Context, tx pgx.Tx, name, tenant, token string, quota int, status string, feats ...string) error {
	t.Helper()
	hash := auth.HashToken(token)
	_, err := tx.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features)
		 VALUES ($1,$2,$3,$4,$5,'sports',$6)`,
		name, tenant, hash, quota, status, feats,
	)
	return err
}

func doReq(t *testing.T, h http.Handler, method, path, token, body string) *http.Response {
	return doReqKey(t, h, method, path, token, body, "")
}

func doReqKey(t *testing.T, h http.Handler, method, path, token, body, idemKey string) *http.Response {
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
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
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

// seedBooking creates a booking (price 100000) for tenant via admin pool.
func seedBooking(t *testing.T, ctx context.Context, admin *pgxpool.Pool, tenant string) (facilityID, bookingID string) {
	t.Helper()
	if err := admin.QueryRow(ctx,
		`INSERT INTO sports_facilities (tenant_id, name, type, price_per_hour) VALUES ($1,'Court 1','badminton',50000) RETURNING id::text`,
		tenant).Scan(&facilityID); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx,
		`INSERT INTO sports_bookings (tenant_id, facility_id, date, start_time, end_time, customer_name, price)
		 VALUES ($1,$2::uuid,'2026-09-15','18:00','20:00','Alice',100000) RETURNING id::text`,
		tenant, facilityID).Scan(&bookingID); err != nil {
		t.Fatal(err)
	}
	return
}

func bookingStatus(t *testing.T, ctx context.Context, admin *pgxpool.Pool, bookingID string) string {
	t.Helper()
	var s string
	if err := admin.QueryRow(ctx,
		`SELECT payment_status FROM sports_bookings WHERE id=$1::uuid`, bookingID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestPayments_RecalcTrigger is THE test: partial → partial, full → paid,
// delete → back to unpaid.
func TestPayments_RecalcTrigger(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, bookingID := seedBooking(t, ctx, h.adminPool, "pay-a")

	// Pay 40000 of 100000 → partial.
	resp := doReq(t, h.router, "POST", "/payments", "tok-pay-a",
		`{"booking_id":"`+bookingID+`","amount":40000,"method":"cash"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("pay1 status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	payID, _ := m["id"].(string)
	if s := bookingStatus(t, ctx, h.adminPool, bookingID); s != "partial" {
		t.Errorf("after 40k: status=%s want partial", s)
	}

	// Pay the remaining 60000 → paid.
	resp2 := doReq(t, h.router, "POST", "/payments", "tok-pay-a",
		`{"booking_id":"`+bookingID+`","amount":60000,"method":"qris"}`)
	if resp2.StatusCode != 201 {
		t.Fatalf("pay2 status=%d", resp2.StatusCode)
	}
	if s := bookingStatus(t, ctx, h.adminPool, bookingID); s != "paid" {
		t.Errorf("after full: status=%s want paid", s)
	}

	// Delete the 60000 → back to partial.
	resp3 := doReq(t, h.router, "DELETE", "/payments/"+payID, "tok-pay-a", "")
	if resp3.StatusCode != 200 {
		t.Fatalf("del status=%d", resp3.StatusCode)
	}
	if s := bookingStatus(t, ctx, h.adminPool, bookingID); s != "partial" {
		t.Errorf("after delete: status=%s want partial", s)
	}
	// Tombstone, not gone: getOne 404s, list excludes, row keeps who/when.
	if r := doReq(t, h.router, "GET", "/payments/"+payID, "tok-pay-a", ""); r.StatusCode != 404 {
		t.Errorf("get deleted status=%d want 404", r.StatusCode)
		readAll(t, r)
	}
	mL := readJSON(t, doReq(t, h.router, "GET", "/payments", "tok-pay-a", ""))
	items, _ := mL["items"].([]any)
	for _, it := range items {
		if m, _ := it.(map[string]any); m["id"] == payID {
			t.Error("deleted payment still listed")
		}
	}
	var delBy string
	var delAt time.Time
	if err := h.adminPool.QueryRow(ctx,
		`SELECT deleted_by, deleted_at FROM sports_payments WHERE id=$1::uuid`, payID).Scan(&delBy, &delAt); err != nil {
		t.Fatalf("tombstone row missing: %v", err)
	}
	if delAt.IsZero() {
		t.Error("deleted_at not set")
	}
}

// TestPayments_Isolation: B cannot see/pay A's booking.
func TestPayments_Isolation(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, bookingID := seedBooking(t, ctx, h.adminPool, "pay-a")

	resp := doReq(t, h.router, "POST", "/payments", "tok-pay-b",
		`{"booking_id":"`+bookingID+`","amount":10000}`)
	if resp.StatusCode != 404 {
		t.Errorf("B pay on A's booking status=%d want 404", resp.StatusCode)
	}
	// B's list sees nothing.
	respL := doReq(t, h.router, "GET", "/payments", "tok-pay-b", "")
	mL := readJSON(t, respL)
	items, _ := mL["items"].([]any)
	if len(items) != 0 {
		t.Errorf("B saw %d payments, want 0", len(items))
	}
}

// TestPayments_Idempotency: same Idempotency-Key twice → one row, second
// call returns the original id with deduplicated:true. No key → two rows
// (optional phase: absent header behaves exactly as before).
func TestPayments_Idempotency(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, bookingID := seedBooking(t, ctx, h.adminPool, "pay-a")

	body := `{"booking_id":"` + bookingID + `","amount":25000,"method":"cash"}`
	resp := doReqKey(t, h.router, "POST", "/payments", "tok-pay-a", body, "key-1")
	if resp.StatusCode != 201 {
		t.Fatalf("first status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	first := readJSON(t, resp)
	id1, _ := first["id"].(string)

	resp2 := doReqKey(t, h.router, "POST", "/payments", "tok-pay-a", body, "key-1")
	if resp2.StatusCode != 200 {
		t.Fatalf("replay status=%d want 200, body=%s", resp2.StatusCode, readAll(t, resp2))
	}
	second := readJSON(t, resp2)
	if second["id"] != id1 || second["deduplicated"] != true {
		t.Errorf("replay = %v, want same id with deduplicated:true", second)
	}

	var n int
	if err := h.adminPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM sports_payments WHERE tenant_id='pay-a'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("payments rows = %d, want 1 (no double charge)", n)
	}

	// Same body, different key → second charge (keys scope dedup).
	resp3 := doReqKey(t, h.router, "POST", "/payments", "tok-pay-a", body, "key-2")
	if resp3.StatusCode != 201 {
		t.Fatalf("new key status=%d body=%s", resp3.StatusCode, readAll(t, resp3))
	}

	// Malformed key → 400.
	resp4 := doReqKey(t, h.router, "POST", "/payments", "tok-pay-a", body, "bad key!!")
	if resp4.StatusCode != 400 {
		t.Errorf("bad key status=%d want 400", resp4.StatusCode)
	}
	readAll(t, resp4)
}

// TestPayments_Validation: amount <= 0 → 400, bad method → 400.
func TestPayments_Validation(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, bookingID := seedBooking(t, ctx, h.adminPool, "pay-a")

	resp := doReq(t, h.router, "POST", "/payments", "tok-pay-a",
		`{"booking_id":"`+bookingID+`","amount":0}`)
	if resp.StatusCode != 400 {
		t.Errorf("zero amount status=%d want 400", resp.StatusCode)
	}
	resp2 := doReq(t, h.router, "POST", "/payments", "tok-pay-a",
		`{"booking_id":"`+bookingID+`","amount":1000,"method":"bitcoin"}`)
	if resp2.StatusCode != 400 {
		t.Errorf("bad method status=%d want 400", resp2.StatusCode)
	}
}
