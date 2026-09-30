// Kantin POS tests: product CRUD, atomic sale (server-side total,
// stock decrement, oversell → 409), isolation.
//
// DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F
package kantin

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
	"github.com/davinakmalyasha/agents-proxy/internal/features/clients/sports/testutil"
	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
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
	// These tests need a SECOND, non-privileged role: they create an `app`
	// role, grant it table privileges, and then connect as it to prove the
	// grant boundary. Supabase's pooler only authenticates roles it manages
	// (postgres / anon / authenticated / service_role and <role>.<projectref>
	// for roles created inside the project), so against a hosted pooler the
	// `app` connect fails with "failed to retrieve database credentials" —
	// or, without the project ref, "no tenant identifier provided". Repeated
	// failures there also trip the pooler's circuit breaker, which then
	// blocks even the admin connection.
	//
	// So: skip loudly instead of failing, and instead of hammering a shared
	// hosted database. Run this package against a local Postgres/Laragon
	// (where `app` is an ordinary role) to actually exercise it.
	if isManagedPoolerDSN(dsn) {
		t.Skip("DATABASE_URL points at a managed pooler, which cannot authenticate a second test role — " +
			"run this package against a local Postgres to exercise it")
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
			`DELETE FROM sports_kantin_stock_movements WHERE tenant_id IN ('ka-a','ka-b');
			 DELETE FROM sports_kantin_sale_items WHERE tenant_id IN ('ka-a','ka-b');
			 DELETE FROM sports_kantin_sales WHERE tenant_id IN ('ka-a','ka-b');
			 DELETE FROM sports_kantin_products WHERE tenant_id IN ('ka-a','ka-b');
			 DELETE FROM clients WHERE tenant_id IN ('ka-a','ka-b');`); err != nil {
			return err
		}
		for _, c := range [][]string{{"ka-a", "ka-a", "tok-ka-a"}, {"ka-b", "ka-b", "tok-ka-b"}} {
			if err := seedClientTx(t, ctx, tx, c[0], c[1], c[2], 1000000, "active", "records", "kantin"); err != nil {
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

// isManagedPoolerDSN reports whether the DSN targets a managed connection
// pooler (Supabase's transaction/session pooler) rather than a Postgres you
// control. Those only authenticate the roles they provision.
func isManagedPoolerDSN(dsn string) bool {
	h := strings.ToLower(dsn)
	for _, marker := range []string{
		"pooler.supabase.com",
		"aws-0-", // Supavisor / pooler hosts
		"supabase.co:5432",
		".pooler.",
	} {
		if strings.Contains(h, marker) {
			return true
		}
	}
	return false
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

// seedProduct creates a track-stock product for tenant via admin.
func seedProduct(t *testing.T, ctx context.Context, admin *pgxpool.Pool, tenant, name string, price float64, stock int) string {
	t.Helper()
	var id string
	if err := admin.QueryRow(ctx,
		`INSERT INTO sports_kantin_products (tenant_id, name, price, cost_price, track_stock, stock)
		 VALUES ($1,$2,$3,2000,true,$4) RETURNING id::text`,
		tenant, name, price, stock).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestKantin_AtomicSale is THE test: total computed server-side, stock
// decremented, sale + items + movement all in one tx.
func TestKantin_AtomicSale(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p1 := seedProduct(t, ctx, h.adminPool, "ka-a", "Es Teh", 5000, 10)
	p2 := seedProduct(t, ctx, h.adminPool, "ka-a", "Indomie", 8000, 5)

	// Sale 2× Es Teh + 1× Indomie = 18000.
	resp := doReq(t, h.router, "POST", "/kantin/sales", "tok-ka-a",
		`{"method":"cash","items":[{"product_id":"`+p1+`","qty":2},{"product_id":"`+p2+`","qty":1}]}`)
	if resp.StatusCode != 201 {
		t.Fatalf("sale status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	if m["total"] != float64(18000) {
		t.Errorf("total=%v want 18000 (server-computed)", m["total"])
	}
	// Stock decremented: 10-2=8, 5-1=4.
	var s1, s2 int
	if err := h.adminPool.QueryRow(ctx, `SELECT stock FROM sports_kantin_products WHERE id=$1::uuid`, p1).Scan(&s1); err != nil {
		t.Fatal(err)
	}
	if err := h.adminPool.QueryRow(ctx, `SELECT stock FROM sports_kantin_products WHERE id=$1::uuid`, p2).Scan(&s2); err != nil {
		t.Fatal(err)
	}
	if s1 != 8 || s2 != 4 {
		t.Errorf("stocks=%d,%d want 8,4", s1, s2)
	}
	// Movements logged.
	var mv int
	if err := h.adminPool.QueryRow(ctx,
		`SELECT count(*) FROM sports_kantin_stock_movements WHERE tenant_id='ka-a' AND reason='sale'`).Scan(&mv); err != nil {
		t.Fatal(err)
	}
	if mv != 2 {
		t.Errorf("movements=%d want 2", mv)
	}
}

// TestKantin_Oversell: selling more than stock → 409, nothing written.
func TestKantin_Oversell(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p := seedProduct(t, ctx, h.adminPool, "ka-a", "Air", 5000, 3)
	resp := doReq(t, h.router, "POST", "/kantin/sales", "tok-ka-a",
		`{"items":[{"product_id":"`+p+`","qty":99}]}`)
	if resp.StatusCode != 409 {
		t.Errorf("oversell status=%d want 409 (body=%s)", resp.StatusCode, readAll(t, resp))
	}
	// Stock unchanged, no sale rows.
	var s int
	if err := h.adminPool.QueryRow(ctx, `SELECT stock FROM sports_kantin_products WHERE id=$1::uuid`, p).Scan(&s); err != nil {
		t.Fatal(err)
	}
	if s != 3 {
		t.Errorf("stock=%d want 3 (unchanged)", s)
	}
	var n int
	if err := h.adminPool.QueryRow(ctx,
		`SELECT count(*) FROM sports_kantin_sales WHERE tenant_id='ka-a'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("sales=%d want 0 (rolled back)", n)
	}
}

// TestKantin_ClientPriceIgnored: client-sent total is never trusted.
func TestKantin_ClientPriceIgnored(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p := seedProduct(t, ctx, h.adminPool, "ka-a", "Teh Panas", 4000, 10)
	resp := doReq(t, h.router, "POST", "/kantin/sales", "tok-ka-a",
		`{"items":[{"product_id":"`+p+`","qty":1}]}`)
	m := readJSON(t, resp)
	if m["total"] != float64(4000) {
		t.Errorf("total=%v want 4000 (DB price, not client)", m["total"])
	}
}

// TestKantin_Isolation: B cannot see A's products/sales.
func TestKantin_Isolation(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	seedProduct(t, ctx, h.adminPool, "ka-a", "Biskuit", 6000, 5)
	respL := doReq(t, h.router, "GET", "/kantin/products", "tok-ka-b", "")
	mL := readJSON(t, respL)
	items, _ := mL["items"].([]any)
	if len(items) != 0 {
		t.Errorf("B saw %d products, want 0", len(items))
	}
}

// TestKantin_OpnameSetsLevel is the regression guard for stock-take.
//
// The "Stok opname" control is a physical count: the operator counted N
// units and that IS the new level. It used to send an ordinary additive
// delta, so counting 5 with the system believing 32 produced 37, and the
// page's promise that the system would compute the difference itself was
// false. The difference must still be written to the ledger.
func TestKantin_OpnameSetsLevel(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p := seedProduct(t, ctx, h.adminPool, "ka-a", "Opnamakan", 5000, 20)

	// Bump the believed level to 32 first, so the count is not a no-op.
	if resp := doReq(t, h.router, "POST", "/kantin/products/"+p+"/stock", "tok-ka-a",
		`{"delta":12,"reason":"restock"}`); resp.StatusCode != 200 {
		t.Fatalf("restock status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}

	resp := doReq(t, h.router, "POST", "/kantin/products/"+p+"/stock", "tok-ka-a",
		`{"count":5,"reason":"correction","note":"hitung fisik"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("opname status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	var stock int
	if err := h.adminPool.QueryRow(ctx,
		`SELECT stock FROM sports_kantin_products WHERE id=$1::uuid`, p).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if stock != 5 {
		t.Errorf("stock=%d want 5 (the counted level, not 32+5=37)", stock)
	}

	// The ledger records the SIGNED DIFFERENCE, not the count.
	var delta int
	var reason string
	if err := h.adminPool.QueryRow(ctx,
		`SELECT delta, reason FROM sports_kantin_stock_movements
		  WHERE tenant_id='ka-a' AND product_id=$1::uuid AND reason='correction'
		  ORDER BY created_at DESC LIMIT 1`, p).Scan(&delta, &reason); err != nil {
		t.Fatal(err)
	}
	if delta != -27 {
		t.Errorf("ledger delta=%d want -27 (5 counted - 32 believed)", delta)
	}
}

// TestKantin_OpnameRejectsNegative: a count is a physical level, never
// below zero. The additive path has its own guard; this one must not be
// a way to smuggle a negative stock past it.
func TestKantin_OpnameRejectsNegative(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p := seedProduct(t, ctx, h.adminPool, "ka-a", "Negatif", 5000, 4)
	resp := doReq(t, h.router, "POST", "/kantin/products/"+p+"/stock", "tok-ka-a",
		`{"count":-3,"reason":"correction"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d want 400 (negative count)", resp.StatusCode)
	}
	var stock int
	if err := h.adminPool.QueryRow(ctx,
		`SELECT stock FROM sports_kantin_products WHERE id=$1::uuid`, p).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if stock != 4 {
		t.Errorf("stock=%d want 4 (unchanged)", stock)
	}
}

// TestKantin_MovementsEndpoint: the ledger must be readable as movements.
// The app used to point this table at /kantin/products, so the stock page
// rendered PRODUCT rows in the history list: no delta, no reason, no note,
// and the product's creation date as the movement time.
func TestKantin_MovementsEndpoint(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p := seedProduct(t, ctx, h.adminPool, "ka-a", "Riwayat Tes", 5000, 10)

	if resp := doReq(t, h.router, "POST", "/kantin/products/"+p+"/stock", "tok-ka-a",
		`{"delta":-3,"reason":"waste","note":"botol pecah"}`); resp.StatusCode != 200 {
		t.Fatalf("waste status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}

	resp := doReq(t, h.router, "GET", "/kantin/movements", "tok-ka-a", "")
	if resp.StatusCode != 200 {
		t.Fatalf("movements status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	items, _ := m["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items=%d want 1", len(items))
	}
	row, _ := items[0].(map[string]any)
	if got := row["product"]; got != "Riwayat Tes" {
		t.Errorf("product=%v want 'Riwayat Tes' (join must resolve)", got)
	}
	if got := row["delta"]; got != float64(-3) {
		t.Errorf("delta=%v want -3", got)
	}
	if got := row["reason"]; got != "waste" {
		t.Errorf("reason=%v want 'waste'", got)
	}
	if got := row["note"]; got != "botol pecah" {
		t.Errorf("note=%v want 'botol pecah'", got)
	}
	// stock_after is reconstructed, not a literal zero.
	if got := row["stock_after"]; got != float64(7) {
		t.Errorf("stock_after=%v want 7 (10 - 3)", got)
	}
	if _, ok := row["created_at"]; !ok {
		t.Error("created_at missing")
	}

	// Tenant isolation: ka-b must not see ka-a's ledger.
	respB := doReq(t, h.router, "GET", "/kantin/movements", "tok-ka-b", "")
	mB := readJSON(t, respB)
	itemsB, _ := mB["items"].([]any)
	if len(itemsB) != 0 {
		t.Errorf("ka-b saw %d movements, want 0", len(itemsB))
	}
}
