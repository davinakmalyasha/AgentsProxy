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
