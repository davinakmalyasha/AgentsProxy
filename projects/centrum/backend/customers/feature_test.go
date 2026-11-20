// Customers feature tests: CRUD + visits aggregation + isolation.
//
// DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F
package customers

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
	"github.com/davinakmalyasha/agents-proxy/projects/centrum/backend/testutil"
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
	if err := db.MigrateAll(ctx, adminPool); err != nil {
		t.Fatal(err)
	}
	if err := testutil.LockAndRun(ctx, adminPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM project_centrum.sports_customers WHERE tenant_id IN ('cu-a','cu-b');
			 DELETE FROM clients WHERE tenant_id IN ('cu-a','cu-b');`); err != nil {
			return err
		}
		for _, c := range [][]string{{"cu-a", "cu-a", "tok-cu-a"}, {"cu-b", "cu-b", "tok-cu-b"}} {
			if err := seedClientTx(t, ctx, tx, c[0], c[1], c[2], 1000000, "active", "records", "customers"); err != nil {
				return err
			}
		}
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

func seedClientTx(t *testing.T, ctx context.Context, tx pgx.Tx, name, tenant, token string, quota int, status string, feats ...string) error {
	t.Helper()
	hash := auth.HashToken(token)
	_, err := tx.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
		 VALUES ($1,$2,$3,$4,$5,'sports',$6,'project_centrum')`,
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

func TestCustomers_CRUD(t *testing.T) {
	h := setup(t)
	resp := doReq(t, h.router, "POST", "/customers", "tok-cu-a",
		`{"name":"Budi","phone":"+6281111"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	id, _ := m["id"].(string)
	// Duplicate phone → 409.
	respDup := doReq(t, h.router, "POST", "/customers", "tok-cu-a", `{"name":"Budi2","phone":"+6281111"}`)
	if respDup.StatusCode != 409 {
		t.Errorf("dup phone status=%d want 409", respDup.StatusCode)
	}
	// List shows visits=0.
	resp2 := doReq(t, h.router, "GET", "/customers", "tok-cu-a", "")
	m2 := readJSON(t, resp2)
	items, _ := m2["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items=%d want 1", len(items))
	}
	if v := items[0].(map[string]any)["visits"]; v != float64(0) {
		t.Errorf("visits=%v want 0", v)
	}
	// Get.
	resp3 := doReq(t, h.router, "GET", "/customers/"+id, "tok-cu-a", "")
	if resp3.StatusCode != 200 {
		t.Errorf("get status=%d", resp3.StatusCode)
	}
}

// TestCustomers_VisitsCounted: a confirmed booking with the customer's
// name counts as a visit.
func TestCustomers_VisitsCounted(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	doReq(t, h.router, "POST", "/customers", "tok-cu-a", `{"name":"Alice","phone":"+6282222"}`).Body.Close()
	// Seed a facility + a confirmed booking named Alice.
	var facID string
	if err := h.adminPool.QueryRow(ctx,
		`INSERT INTO project_centrum.sports_facilities (tenant_id, name, type) VALUES ('cu-a','Court 1','badminton') RETURNING id::text`,
	).Scan(&facID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.adminPool.Exec(ctx,
		`INSERT INTO project_centrum.sports_bookings (tenant_id, facility_id, date, start_time, end_time, customer_name, status)
		 VALUES ('cu-a',$1::uuid,'2026-09-20','10:00','11:00','Alice','confirmed')`, facID); err != nil {
		t.Fatal(err)
	}
	resp := doReq(t, h.router, "GET", "/customers", "tok-cu-a", "")
	m := readJSON(t, resp)
	items, _ := m["items"].([]any)
	first := items[0].(map[string]any)
	if first["name"] != "Alice" {
		t.Fatalf("expected Alice, got %v", first["name"])
	}
	if v := first["visits"]; v != float64(1) {
		t.Errorf("visits=%v want 1", v)
	}
}

// TestCustomers_Isolation: B cannot see A's customers.
func TestCustomers_Isolation(t *testing.T) {
	h := setup(t)
	doReq(t, h.router, "POST", "/customers", "tok-cu-a", `{"name":"Budi","phone":"+6281"}`).Body.Close()
	respL := doReq(t, h.router, "GET", "/customers", "tok-cu-b", "")
	mL := readJSON(t, respL)
	items, _ := mL["items"].([]any)
	if len(items) != 0 {
		t.Errorf("B saw %d customers, want 0", len(items))
	}
}

// TestCustomers_TombstoneDelete: DELETE tombstones instead of removing the
// row — getOne 404s, list/top exclude it, patch 404s, but the row survives
// with deleted_at/deleted_by.
func TestCustomers_TombstoneDelete(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp := doReq(t, h.router, "POST", "/customers", "tok-cu-a",
		`{"name":"Tomb","phone":"+6289999"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	id, _ := m["id"].(string)

	// Delete keeps the 200 {"id","status":"deleted"} contract.
	delResp := doReq(t, h.router, "DELETE", "/customers/"+id, "tok-cu-a", "")
	if delResp.StatusCode != 200 {
		t.Fatalf("delete status=%d body=%s", delResp.StatusCode, readAll(t, delResp))
	}
	dm := readJSON(t, delResp)
	if dm["id"] != id || dm["status"] != "deleted" {
		t.Errorf("delete body=%v want {id,status:deleted}", dm)
	}

	// getOne on the tombstone → 404, exactly as a hard delete read.
	if r := doReq(t, h.router, "GET", "/customers/"+id, "tok-cu-a", ""); r.StatusCode != 404 {
		t.Errorf("get deleted status=%d want 404", r.StatusCode)
		readAll(t, r)
	}
	// Deleted rows can't be edited.
	if r := doReq(t, h.router, "PATCH", "/customers/"+id, "tok-cu-a", `{"notes":"x"}`); r.StatusCode != 404 {
		t.Errorf("patch deleted status=%d want 404", r.StatusCode)
		readAll(t, r)
	}
	// List and top both exclude the row.
	mL := readJSON(t, doReq(t, h.router, "GET", "/customers", "tok-cu-a", ""))
	items, _ := mL["items"].([]any)
	for _, it := range items {
		if it.(map[string]any)["id"] == id {
			t.Error("deleted customer still listed")
		}
	}
	mT := readJSON(t, doReq(t, h.router, "GET", "/customers/top", "tok-cu-a", ""))
	topItems, _ := mT["items"].([]any)
	for _, it := range topItems {
		if it.(map[string]any)["id"] == id {
			t.Error("deleted customer still in top")
		}
	}

	// The row is still in the DB with who/when. Client-token callers
	// carry no staff UserID, so deleted_by is "" — the caller identity,
	// exactly as the payments tombstone stores it.
	var delBy string
	var delAt time.Time
	if err := h.adminPool.QueryRow(ctx,
		`SELECT deleted_by, deleted_at FROM project_centrum.sports_customers WHERE id=$1::uuid`, id).Scan(&delBy, &delAt); err != nil {
		t.Fatalf("tombstone row missing: %v", err)
	}
	if delAt.IsZero() {
		t.Error("deleted_at not set")
	}
	if delBy != "" {
		t.Errorf("deleted_by=%q want caller identity (empty for client token)", delBy)
	}

	// Second delete finds no live row → 404.
	if r := doReq(t, h.router, "DELETE", "/customers/"+id, "tok-cu-a", ""); r.StatusCode != 404 {
		t.Errorf("re-delete status=%d want 404", r.StatusCode)
		readAll(t, r)
	}
}
