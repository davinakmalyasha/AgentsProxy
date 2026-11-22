// Expenses feature tests: CRUD + isolation.
//
// DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F
package expenses

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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
	schemaA   string
	schemaB   string
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
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	adminPool, err := db.NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adminPool.Close)
	testutil.Migrate(t, adminPool)
	if _, err := adminPool.Exec(ctx, `DELETE FROM clients WHERE tenant_id IN ('ex-a','ex-b')`); err != nil {
		t.Fatal(err)
	}
	schemaA := testutil.CloneTenantSchema(t, ctx, adminPool, "ex-a")
	schemaB := testutil.CloneTenantSchema(t, ctx, adminPool, "ex-b")
	for _, c := range []struct{ schema, name, tenant, token string }{
		{schemaA, "ex-a", "ex-a", "tok-ex-a"},
		{schemaB, "ex-b", "ex-b", "tok-ex-b"},
	} {
		seedClient(t, ctx, adminPool, c.schema, c.name, c.tenant, c.token, 1000000, "active", "records", "expenses")
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
	fr := features.BuildProjectRouterForTest("centrum", features.BuilderDeps{
		Pool:  pool,
		Cache: cli,
		Auth:  testutil.NewAuth(adminPool, "ex-a", "ex-b"),
	}, "sports")
	return &harness{router: fr, adminPool: adminPool, rdb: rdb, schemaA: schemaA, schemaB: schemaB}
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

func seedClient(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema, name, tenant, token string, quota int, status string, feats ...string) {
	t.Helper()
	hash := auth.HashToken(token)
	if _, err := pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
		 VALUES ($1,$2,$3,$4,$5,'sports',$6,$7)`,
		name, tenant, hash, quota, status, feats, schema,
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
	if err := h.adminPool.QueryRow(ctx, fmt.Sprintf(
		`SELECT deleted_by, deleted_at FROM %s WHERE id=$1::uuid`,
		testutil.Table(h.schemaA, "sports_expenses")), id).Scan(&delBy, &delAt); err != nil {
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
	if err := h.adminPool.QueryRow(ctx, fmt.Sprintf(
		`SELECT COALESCE(SUM(amount),0) FROM %s WHERE tenant_id='ex-a' AND deleted_at IS NULL`,
		testutil.Table(h.schemaA, "sports_expenses"))).Scan(&filtered); err != nil {
		t.Fatal(err)
	}
	if err := h.adminPool.QueryRow(ctx, fmt.Sprintf(
		`SELECT COALESCE(SUM(amount),0) FROM %s WHERE tenant_id='ex-a'`,
		testutil.Table(h.schemaA, "sports_expenses"))).Scan(&all); err != nil {
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

// doReqAuth is doReq with an explicit identity header: "session" sets
// X-Session-Token, "client" sets X-Client-Token.
func doReqAuth(t *testing.T, h http.Handler, method, path, kind, token, body string) *http.Response {
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
	switch kind {
	case "session":
		req.Header.Set("X-Session-Token", token)
	case "client":
		req.Header.Set("X-Client-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestExpenses_RoleMatrix pins the owner-only expense surface (deferred
// RBAC tightening): list/getOne are owner now; create was already owner,
// and client tokens keep the documented integration access.
func TestExpenses_RoleMatrix(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	testutil.SeedStaffSession(t, ctx, h.adminPool, h.schemaA, "ex-a", "owner@ex.test", "owner", "sess-ex-owner")
	testutil.SeedStaffSession(t, ctx, h.adminPool, h.schemaA, "ex-a", "staff@ex.test", "pegawai", "sess-ex-staff")

	// Seed one expense as the client token (client passes the owner tier
	// by design — the integration model).
	seedResp := doReq(t, h.router, "POST", "/expenses", "tok-ex-a",
		`{"category":"listrik","amount":15000,"note":"matrix"}`)
	if seedResp.StatusCode != 201 {
		t.Fatalf("seed expense status=%d body=%s", seedResp.StatusCode, readAll(t, seedResp))
	}
	expID, _ := readJSON(t, seedResp)["id"].(string)
	if expID == "" {
		t.Fatal("seed expense id missing")
	}

	cases := []struct {
		name, method, path, kind, token, body string
		want                                  int
	}{
		{"list owner", "GET", "/expenses", "session", "sess-ex-owner", "", 200},
		{"list pegawai", "GET", "/expenses", "session", "sess-ex-staff", "", 403},
		{"list client", "GET", "/expenses", "client", "tok-ex-a", "", 200},
		{"get owner", "GET", "/expenses/" + expID, "session", "sess-ex-owner", "", 200},
		{"get pegawai", "GET", "/expenses/" + expID, "session", "sess-ex-staff", "", 403},
		{"get client", "GET", "/expenses/" + expID, "client", "tok-ex-a", "", 200},
		{"patch owner", "PATCH", "/expenses/" + expID, "session", "sess-ex-owner", `{"amount":16000}`, 200},
		{"patch pegawai", "PATCH", "/expenses/" + expID, "session", "sess-ex-staff", `{"amount":16000}`, 403},
		{"create pegawai", "POST", "/expenses", "session", "sess-ex-staff",
			`{"category":"lainnya","amount":1000}`, 403},
		{"create owner", "POST", "/expenses", "session", "sess-ex-owner",
			`{"category":"lainnya","amount":1000}`, 201},
	}
	for _, tc := range cases {
		resp := doReqAuth(t, h.router, tc.method, tc.path, tc.kind, tc.token, tc.body)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status=%d want %d body=%s", tc.name, resp.StatusCode, tc.want, readAll(t, resp))
			continue
		}
		resp.Body.Close()
	}
}
