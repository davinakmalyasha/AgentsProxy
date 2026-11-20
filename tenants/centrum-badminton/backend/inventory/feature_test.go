// Mandatory isolation + CRUD + quantity-guard tests for the
// inventory feature.
//
// DONE: Phase 4 (sports vertical) / STANDARDS A, D, F
package inventory

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
	dsn := os.Getenv("TEST_DATABASE_URL")
	rurl := os.Getenv("TEST_REDIS_URL")
	if dsn == "" || rurl == "" {
		t.Skip("TEST_DATABASE_URL / TEST_REDIS_URL not set — skipping integration test")
	}
	adminDSN := os.Getenv("TEST_DATABASE_ADMIN_URL")
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
	if err := db.Migrate(ctx, adminPool); err != nil {
		t.Fatal(err)
	}
	if err := testutil.LockAndRun(ctx, adminPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM sports_inventory WHERE tenant_id IN ('inv-a','inv-b');
			 DELETE FROM clients WHERE tenant_id IN ('inv-a','inv-b');`); err != nil {
			return err
		}
		seedClientTx(t, ctx, tx, "inv-a", "inv-a", "tok-inv-a", 1000000, "active", "records", "inventory")
		seedClientTx(t, ctx, tx, "inv-b", "inv-b", "tok-inv-b", 1000000, "active", "records", "inventory")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pool, err := db.NewPool(ctx, toRuntimeDSN(dsn))
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

func toRuntimeDSN(dsn string) string {
	i := strings.Index(dsn, "://")
	if i < 0 {
		return dsn
	}
	rest := dsn[i+3:]
	at := strings.Index(rest, "@")
	if at < 0 {
		return dsn
	}
	return dsn[:i+3] + "app_runtime" + rest[at:]
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

func TestInventory_CRUD(t *testing.T) {
	h := setup(t)
	resp := doReq(t, h.router, "POST", "/inventory", "tok-inv-a",
		`{"name":"Soda","quantity":10,"unit_price":8000,"low_stock_threshold":5}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatal("no id")
	}
	// List — should show low_stock=false (10 > 5).
	resp2 := doReq(t, h.router, "GET", "/inventory", "tok-inv-a", "")
	m2 := readJSON(t, resp2)
	items, _ := m2["items"].([]any)
	if len(items) != 1 {
		t.Errorf("items=%d want 1", len(items))
	}
	first := items[0].(map[string]any)
	if first["low_stock"] != false {
		t.Errorf("low_stock=%v want false", first["low_stock"])
	}
	// Adjust down to 2 (below threshold).
	resp3 := doReq(t, h.router, "POST", "/inventory/"+id+"/adjust", "tok-inv-a", `{"delta":-8}`)
	if resp3.StatusCode != 200 {
		t.Fatalf("adjust status=%d body=%s", resp3.StatusCode, readAll(t, resp3))
	}
	m3 := readJSON(t, resp3)
	if m3["quantity"] != float64(2) {
		t.Errorf("quantity=%v want 2", m3["quantity"])
	}
	// Now low_stock should be true.
	resp4 := doReq(t, h.router, "GET", "/inventory/"+id, "tok-inv-a", "")
	m4 := readJSON(t, resp4)
	if m4["low_stock"] != true {
		t.Errorf("low_stock=%v want true", m4["low_stock"])
	}
}

// TestInventory_NeverNegative: selling more than stock → 409.
func TestInventory_NeverNegative(t *testing.T) {
	h := setup(t)
	resp := doReq(t, h.router, "POST", "/inventory", "tok-inv-a", `{"name":"Water","quantity":3}`)
	m := readJSON(t, resp)
	id, _ := m["id"].(string)
	// Sell 5 of 3 → 409, quantity unchanged.
	resp2 := doReq(t, h.router, "POST", "/inventory/"+id+"/adjust", "tok-inv-a", `{"delta":-5}`)
	if resp2.StatusCode != 409 {
		t.Errorf("oversell status=%d want 409", resp2.StatusCode)
	}
	// Quantity still 3.
	resp3 := doReq(t, h.router, "GET", "/inventory/"+id, "tok-inv-a", "")
	m3 := readJSON(t, resp3)
	if m3["quantity"] != float64(3) {
		t.Errorf("quantity=%v want 3 (must never go negative)", m3["quantity"])
	}
}

// TestInventory_TenantIsolation — the mandatory isolation test.
func TestInventory_TenantIsolation(t *testing.T) {
	h := setup(t)
	resp := doReq(t, h.router, "POST", "/inventory", "tok-inv-a", `{"name":"A Item"}`)
	m := readJSON(t, resp)
	id, _ := m["id"].(string)
	respL := doReq(t, h.router, "GET", "/inventory", "tok-inv-b", "")
	mL := readJSON(t, respL)
	items, _ := mL["items"].([]any)
	if len(items) != 0 {
		t.Errorf("B saw %d items, want 0", len(items))
	}
	respG := doReq(t, h.router, "GET", "/inventory/"+id, "tok-inv-b", "")
	if respG.StatusCode != 404 {
		t.Errorf("B get status=%d want 404", respG.StatusCode)
	}
	respAdj := doReq(t, h.router, "POST", "/inventory/"+id+"/adjust", "tok-inv-b", `{"delta":-1}`)
	if respAdj.StatusCode != 409 {
		t.Errorf("B adjust status=%d want 409", respAdj.StatusCode)
	}
}
