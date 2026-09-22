package records

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/auth"
	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
)

// recordsHarness wires a router with the records feature mounted
// under a single vertical. The runtime pool connects as the
// non-superuser `app` role so RLS applies — this is how production
// runs and the only way to catch cross-tenant leaks.
type recordsHarness struct {
	router http.Handler
	pool   *pgxpool.Pool
	rdb    *redis.Client
}

func setupRecords(t *testing.T) *recordsHarness {
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

	// Admin pool (superuser) for migrations + role bootstrap + seed.
	adminPool, err := db.NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adminPool.Close)
	if err := bootstrapAppRole(ctx, adminPool); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, adminPool); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, `DELETE FROM clients WHERE tenant_id IN ('tenant-a','tenant-b','tenant-c')`); err != nil {
		t.Fatal(err)
	}

	// Runtime pool connects as `app` so RLS applies.
	pool, err := db.NewPool(ctx, toAppDSN(dsn))
	if err != nil {
		t.Fatalf("connect as app: %v", err)
	}
	t.Cleanup(pool.Close)
	// Cleanup must run as the superuser (adminPool) to bypass RLS —
	// otherwise the `app` role's RLS would hide the rows it owns.
	if _, err := adminPool.Exec(ctx, `DELETE FROM tenant_records WHERE tenant_id IN ('tenant-a','tenant-b','tenant-c')`); err != nil {
		t.Fatal(err)
	}

	// Seed clients (admin role has INSERT).
	seedClient(t, ctx, adminPool, "tenant-a", "tenant-a", "tok-a", 1000000, "active", "fnb", []string{"records", "menu"})
	seedClient(t, ctx, adminPool, "tenant-b", "tenant-b", "tok-b", 1000000, "active", "fnb", []string{"records"})
	seedClient(t, ctx, adminPool, "tenant-c", "tenant-c", "tok-c", 1000000, "paused", "fnb", []string{"records"})

	rdb, err := redisx.NewClient(ctx, rurl)
	if err != nil {
		t.Fatal(err)
	}
	rdb.FlushDB(ctx)
	t.Cleanup(func() { rdb.Close() })

	cli := &cache.Client{DB: pool, RDB: rdb}
	recDeps := Deps{Pool: pool, Cache: cli}
	rec := New(recDeps)
	router := rec.Routes("fnb")
	wrapped := WithClientAuth(recDeps)(router)
	return &recordsHarness{router: wrapped, pool: pool, rdb: rdb}
}

func bootstrapAppRole(ctx context.Context, admin *pgxpool.Pool) error {
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
			return err
		}
	}
	return nil
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

func seedClient(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name, tenant, token string, quota int, status, vertical string, features []string) {
	t.Helper()
	hash := auth.HashToken(token)
	if _, err := pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		name, tenant, hash, quota, status, vertical, features,
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

// ---- tests ----

func TestRecords_CreateAndList(t *testing.T) {
	h := setupRecords(t)
	body := `{"kind":"item","title":"Nasi Goreng","payload":{"price":25000}}`
	resp := doReq(t, h.router, "POST", "/", "tok-a", body)
	if resp.StatusCode != 201 {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatal("create: no id in response")
	}
	resp2 := doReq(t, h.router, "GET", "/", "tok-a", "")
	if resp2.StatusCode != 200 {
		t.Fatalf("list status=%d", resp2.StatusCode)
	}
	m2 := readJSON(t, resp2)
	items, _ := m2["items"].([]any)
	if len(items) != 1 {
		t.Errorf("items=%d want 1", len(items))
	}
	if v, ok := m2["has_more"].(bool); !ok || v {
		t.Errorf("has_more=%v want false", m2["has_more"])
	}
}

func TestRecords_GetOne(t *testing.T) {
	h := setupRecords(t)
	resp := doReq(t, h.router, "POST", "/", "tok-a", `{"kind":"item","title":"Bakso","payload":{"price":20000}}`)
	m := readJSON(t, resp)
	id, _ := m["id"].(string)
	resp2 := doReq(t, h.router, "GET", "/"+id, "tok-a", "")
	if resp2.StatusCode != 200 {
		t.Errorf("get status=%d", resp2.StatusCode)
	}
}

func TestRecords_PatchAndDelete(t *testing.T) {
	h := setupRecords(t)
	resp := doReq(t, h.router, "POST", "/", "tok-a", `{"kind":"item","title":"Sate","payload":{}}`)
	m := readJSON(t, resp)
	id, _ := m["id"].(string)
	resp2 := doReq(t, h.router, "PATCH", "/"+id, "tok-a", `{"title":"Sate Madura","payload":{"price":30000}}`)
	if resp2.StatusCode != 200 {
		t.Errorf("patch status=%d", resp2.StatusCode)
	}
	resp3 := doReq(t, h.router, "DELETE", "/"+id, "tok-a", "")
	if resp3.StatusCode != 200 {
		t.Errorf("delete status=%d", resp3.StatusCode)
	}
	resp4 := doReq(t, h.router, "GET", "/"+id+"?status=deleted", "tok-a", "")
	if resp4.StatusCode != 200 {
		t.Errorf("get-after-delete status=%d", resp4.StatusCode)
	}
	m4 := readJSON(t, resp4)
	if m4["status"] != "deleted" {
		t.Errorf("status=%v want deleted", m4["status"])
	}
}

// TestRecords_TenantIsolation: A creates, B sees nothing.
func TestRecords_TenantIsolation(t *testing.T) {
	h := setupRecords(t)
	resp := doReq(t, h.router, "POST", "/", "tok-a", `{"kind":"item","title":"A's Menu","payload":{"price":10000}}`)
	m := readJSON(t, resp)
	id, _ := m["id"].(string)
	respL := doReq(t, h.router, "GET", "/", "tok-b", "")
	if respL.StatusCode != 200 {
		t.Fatalf("B list status=%d", respL.StatusCode)
	}
	mL := readJSON(t, respL)
	items, _ := mL["items"].([]any)
	if len(items) != 0 {
		t.Errorf("B saw %d items, expected 0", len(items))
	}
	respG := doReq(t, h.router, "GET", "/"+id, "tok-b", "")
	if respG.StatusCode != 404 {
		t.Errorf("B get-by-id status=%d want 404", respG.StatusCode)
	}
	respP := doReq(t, h.router, "PATCH", "/"+id, "tok-b", `{"title":"hijack"}`)
	if respP.StatusCode != 404 {
		t.Errorf("B patch status=%d want 404", respP.StatusCode)
	}
	respD := doReq(t, h.router, "DELETE", "/"+id, "tok-b", "")
	if respD.StatusCode != 404 {
		t.Errorf("B delete status=%d want 404", respD.StatusCode)
	}
}

func TestRecords_MissingToken(t *testing.T) {
	h := setupRecords(t)
	resp := doReq(t, h.router, "POST", "/", "", `{"kind":"item","title":"x","payload":{}}`)
	if resp.StatusCode != 401 {
		t.Errorf("status=%d want 401", resp.StatusCode)
	}
}

func TestRecords_PausedClient(t *testing.T) {
	h := setupRecords(t)
	resp := doReq(t, h.router, "POST", "/", "tok-c", `{"kind":"item","title":"x","payload":{}}`)
	if resp.StatusCode != 403 {
		t.Errorf("paused status=%d want 403", resp.StatusCode)
	}
}

func TestRecords_InvalidToken(t *testing.T) {
	h := setupRecords(t)
	resp := doReq(t, h.router, "GET", "/", "not-a-real-token", "")
	if resp.StatusCode != 401 {
		t.Errorf("status=%d want 401", resp.StatusCode)
	}
}

func TestRecords_KindRequired(t *testing.T) {
	h := setupRecords(t)
	resp := doReq(t, h.router, "POST", "/", "tok-a", `{"title":"x","payload":{}}`)
	if resp.StatusCode != 400 {
		t.Errorf("status=%d want 400", resp.StatusCode)
	}
}

// TestRecords_CursorPagination: create 5, page through with limit=2.
func TestRecords_CursorPagination(t *testing.T) {
	h := setupRecords(t)
	for i := 0; i < 5; i++ {
		resp := doReq(t, h.router, "POST", "/", "tok-a",
			`{"kind":"item","title":"item-`+strconv.Itoa(i)+`","payload":{}}`)
		resp.Body.Close()
	}
	resp := doReq(t, h.router, "GET", "/?limit=2", "tok-a", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	m := readJSON(t, resp)
	items, _ := m["items"].([]any)
	if len(items) != 2 {
		t.Errorf("page1 items=%d want 2", len(items))
	}
	if v, _ := m["has_more"].(bool); !v {
		t.Errorf("has_more=false want true")
	}
	cursor, _ := m["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("next_cursor missing")
	}
	// Use the cursor to fetch the next page.
	resp2 := doReq(t, h.router, "GET", "/?limit=2&before="+cursor, "tok-a", "")
	if resp2.StatusCode != 200 {
		t.Fatalf("page2 status=%d", resp2.StatusCode)
	}
	m2 := readJSON(t, resp2)
	items2, _ := m2["items"].([]any)
	if len(items2) != 2 {
		t.Errorf("page2 items=%d want 2", len(items2))
	}
	if v, _ := m2["has_more"].(bool); !v {
		t.Errorf("page2 has_more=false want true")
	}
	// Page 3 should have 1 item and no more.
	cursor2, _ := m2["next_cursor"].(string)
	if cursor2 == "" {
		t.Fatal("page2 next_cursor missing")
	}
	resp3 := doReq(t, h.router, "GET", "/?limit=2&before="+cursor2, "tok-a", "")
	m3 := readJSON(t, resp3)
	items3, _ := m3["items"].([]any)
	if len(items3) != 1 {
		t.Errorf("page3 items=%d want 1", len(items3))
	}
	if v, _ := m3["has_more"].(bool); v {
		t.Errorf("page3 has_more=true want false")
	}
}

// TestRecords_SoftDeleteHidden: deleted rows are excluded by default.
func TestRecords_SoftDeleteHidden(t *testing.T) {
	h := setupRecords(t)
	resp := doReq(t, h.router, "POST", "/", "tok-a", `{"kind":"item","title":"to delete","payload":{}}`)
	m := readJSON(t, resp)
	id, _ := m["id"].(string)
	// Soft-delete.
	del := doReq(t, h.router, "DELETE", "/"+id, "tok-a", "")
	del.Body.Close()
	// List by default — should be 0.
	r1 := doReq(t, h.router, "GET", "/", "tok-a", "")
	m1 := readJSON(t, r1)
	items1, _ := m1["items"].([]any)
	if len(items1) != 0 {
		t.Errorf("default list items=%d want 0", len(items1))
	}
	// List with ?status=deleted — should be 1.
	r2 := doReq(t, h.router, "GET", "/?status=deleted", "tok-a", "")
	m2 := readJSON(t, r2)
	items2, _ := m2["items"].([]any)
	if len(items2) != 1 {
		t.Errorf("status=deleted items=%d want 1", len(items2))
	}
	// get-one without status filter — 404.
	r3 := doReq(t, h.router, "GET", "/"+id, "tok-a", "")
	if r3.StatusCode != 404 {
		t.Errorf("get deleted status=%d want 404", r3.StatusCode)
	}
	// get-one with ?status=deleted — 200.
	r4 := doReq(t, h.router, "GET", "/"+id+"?status=deleted", "tok-a", "")
	if r4.StatusCode != 200 {
		t.Errorf("get ?status=deleted status=%d want 200", r4.StatusCode)
	}
}

func TestRecords_SearchByKind(t *testing.T) {
	h := setupRecords(t)
	doReq(t, h.router, "POST", "/", "tok-a", `{"kind":"item","title":"Nasi","payload":{}}`).Body.Close()
	doReq(t, h.router, "POST", "/", "tok-a", `{"kind":"service","title":"Delivery","payload":{}}`).Body.Close()
	resp := doReq(t, h.router, "GET", "/?kind=item", "tok-a", "")
	m := readJSON(t, resp)
	items, _ := m["items"].([]any)
	if len(items) != 1 {
		t.Errorf("items=%d want 1", len(items))
	}
}

func TestRecords_SearchByQuery(t *testing.T) {
	h := setupRecords(t)
	doReq(t, h.router, "POST", "/", "tok-a", `{"kind":"item","title":"Nasi Goreng","payload":{}}`).Body.Close()
	doReq(t, h.router, "POST", "/", "tok-a", `{"kind":"item","title":"Bakso","payload":{}}`).Body.Close()
	resp := doReq(t, h.router, "GET", "/?q=nasi", "tok-a", "")
	m := readJSON(t, resp)
	items, _ := m["items"].([]any)
	if len(items) != 1 {
		t.Errorf("q=nasi items=%d want 1", len(items))
	}
}

// TestRecords_InvalidStatusFilter: status=foo is allowed (we don't
// whitelist — clients can pass arbitrary status filters; the
// default for list is "active", so unknown values return 0 items).
func TestRecords_StatusUnknownReturnsZero(t *testing.T) {
	h := setupRecords(t)
	doReq(t, h.router, "POST", "/", "tok-a", `{"kind":"item","title":"x","payload":{}}`).Body.Close()
	resp := doReq(t, h.router, "GET", "/?status=nosuch", "tok-a", "")
	m := readJSON(t, resp)
	items, _ := m["items"].([]any)
	if len(items) != 0 {
		t.Errorf("status=nosuch items=%d want 0", len(items))
	}
}
