// Expenses feature tests: CRUD + isolation.
//
// DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F
package expenses

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
			`DELETE FROM sports_expenses WHERE tenant_id IN ('ex-a','ex-b');
			 DELETE FROM clients WHERE tenant_id IN ('ex-a','ex-b');`); err != nil {
			return err
		}
		for _, c := range [][]string{{"ex-a", "ex-a", "tok-ex-a"}, {"ex-b", "ex-b", "tok-ex-b"}} {
			if err := seedClientTx(t, ctx, tx, c[0], c[1], c[2], 1000000, "active", "records", "expenses"); err != nil {
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
			`GRANT SELECT ON tenant_schema_contracts TO app`,
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

func TestExpenses_CRUD(t *testing.T) {
	h := setup(t)
	resp := doReq(t, h.router, "POST", "/expenses", "tok-ex-a",
		`{"category":"listrik","amount":500000,"note":"tagihan bulan"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	id, _ := m["id"].(string)
	// List.
	resp2 := doReq(t, h.router, "GET", "/expenses", "tok-ex-a", "")
	m2 := readJSON(t, resp2)
	items, _ := m2["items"].([]any)
	if len(items) != 1 {
		t.Errorf("items=%d want 1", len(items))
	}
	// Get.
	resp3 := doReq(t, h.router, "GET", "/expenses/"+id, "tok-ex-a", "")
	if resp3.StatusCode != 200 {
		t.Errorf("get status=%d", resp3.StatusCode)
	}
	// Patch.
	resp4 := doReq(t, h.router, "PATCH", "/expenses/"+id, "tok-ex-a", `{"amount":550000}`)
	if resp4.StatusCode != 200 {
		t.Errorf("patch status=%d", resp4.StatusCode)
	}
	// Delete.
	resp5 := doReq(t, h.router, "DELETE", "/expenses/"+id, "tok-ex-a", "")
	if resp5.StatusCode != 200 {
		t.Errorf("delete status=%d", resp5.StatusCode)
	}
}

func TestExpenses_Validation(t *testing.T) {
	h := setup(t)
	resp := doReq(t, h.router, "POST", "/expenses", "tok-ex-a", `{"amount":0}`)
	if resp.StatusCode != 400 {
		t.Errorf("zero amount status=%d want 400", resp.StatusCode)
	}
}

// TestExpenses_TombstoneDelete: DELETE tombstones instead of removing the
// row — getOne 404s, list excludes it, patch 404s, but the row survives
// with deleted_at/deleted_by, and the filtered SUM (the shape
// query_net_profit / query_range_summary / reports use) excludes it.
func TestExpenses_TombstoneDelete(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp := doReq(t, h.router, "POST", "/expenses", "tok-ex-a",
		`{"category":"listrik","amount":500000,"note":"tagihan bulan"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	id, _ := m["id"].(string)

	respB := doReq(t, h.router, "POST", "/expenses", "tok-ex-a",
		`{"category":"air","amount":200000}`)
	if respB.StatusCode != 201 {
		t.Fatalf("create2 status=%d body=%s", respB.StatusCode, readAll(t, respB))
	}
	mB := readJSON(t, respB)
	keepID, _ := mB["id"].(string)

	// Delete keeps the 200 {"id","status":"deleted"} contract.
	delResp := doReq(t, h.router, "DELETE", "/expenses/"+id, "tok-ex-a", "")
	if delResp.StatusCode != 200 {
		t.Fatalf("delete status=%d body=%s", delResp.StatusCode, readAll(t, delResp))
	}
	dm := readJSON(t, delResp)
	if dm["id"] != id || dm["status"] != "deleted" {
		t.Errorf("delete body=%v want {id,status:deleted}", dm)
	}

	// getOne on the tombstone → 404, exactly as a hard delete read.
	if r := doReq(t, h.router, "GET", "/expenses/"+id, "tok-ex-a", ""); r.StatusCode != 404 {
		t.Errorf("get deleted status=%d want 404", r.StatusCode)
		readAll(t, r)
	}
	// Deleted rows can't be edited.
	if r := doReq(t, h.router, "PATCH", "/expenses/"+id, "tok-ex-a", `{"amount":1}`); r.StatusCode != 404 {
		t.Errorf("patch deleted status=%d want 404", r.StatusCode)
		readAll(t, r)
	}
	// List excludes the row but keeps the survivor.
	mL := readJSON(t, doReq(t, h.router, "GET", "/expenses", "tok-ex-a", ""))
	items, _ := mL["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items=%d want 1 (survivor only)", len(items))
	}
	if items[0].(map[string]any)["id"] != keepID {
		t.Errorf("list survivor=%v want %s", items[0].(map[string]any)["id"], keepID)
	}

	// The row is still in the DB with who/when. Client-token callers
	// carry no staff UserID, so deleted_by is "" — the caller identity,
	// exactly as the payments tombstone stores it.
	var delBy string
	var delAt time.Time
	if err := h.adminPool.QueryRow(ctx,
		`SELECT deleted_by, deleted_at FROM sports_expenses WHERE id=$1::uuid`, id).Scan(&delBy, &delAt); err != nil {
		t.Fatalf("tombstone row missing: %v", err)
	}
	if delAt.IsZero() {
		t.Error("deleted_at not set")
	}
	if delBy != "" {
		t.Errorf("deleted_by=%q want caller identity (empty for client token)", delBy)
	}

	// Net-profit shape: the filtered SUM sees only the survivor, while
	// the unfiltered total proves the row was preserved, not removed.
	var filtered, all float64
	if err := h.adminPool.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount),0) FROM sports_expenses WHERE tenant_id='ex-a' AND deleted_at IS NULL`).Scan(&filtered); err != nil {
		t.Fatal(err)
	}
	if err := h.adminPool.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount),0) FROM sports_expenses WHERE tenant_id='ex-a'`).Scan(&all); err != nil {
		t.Fatal(err)
	}
	if filtered != 200000 {
		t.Errorf("filtered SUM=%v want 200000 (tombstone excluded)", filtered)
	}
	if all != 700000 {
		t.Errorf("unfiltered SUM=%v want 700000 (row preserved)", all)
	}

	// Second delete finds no live row → 404.
	if r := doReq(t, h.router, "DELETE", "/expenses/"+id, "tok-ex-a", ""); r.StatusCode != 404 {
		t.Errorf("re-delete status=%d want 404", r.StatusCode)
		readAll(t, r)
	}
}

// TestExpenses_Isolation: B cannot see A's expenses.
func TestExpenses_Isolation(t *testing.T) {
	h := setup(t)
	doReq(t, h.router, "POST", "/expenses", "tok-ex-a", `{"amount":100000}`).Body.Close()
	respL := doReq(t, h.router, "GET", "/expenses", "tok-ex-b", "")
	mL := readJSON(t, respL)
	items, _ := mL["items"].([]any)
	if len(items) != 0 {
		t.Errorf("B saw %d expenses, want 0", len(items))
	}
}
