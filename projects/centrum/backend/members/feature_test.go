// Mandatory isolation + CRUD + balance tests for the members feature.
//
// DONE: Phase 4 (sports vertical) / STANDARDS A, D, F
package members

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
	if _, err := adminPool.Exec(ctx, `DELETE FROM clients WHERE tenant_id IN ('mem-a','mem-b')`); err != nil {
		t.Fatal(err)
	}
	schemaA := testutil.CloneTenantSchema(t, ctx, adminPool, "mem-a")
	schemaB := testutil.CloneTenantSchema(t, ctx, adminPool, "mem-b")
	seedClient(t, ctx, adminPool, schemaA, "mem-a", "mem-a", "tok-mem-a", 1000000, "active", "records", "members")
	seedClient(t, ctx, adminPool, schemaB, "mem-b", "mem-b", "tok-mem-b", 1000000, "active", "records", "members")
	adminA := testutil.SchemaPool(t, ctx, adminDSN, schemaA)

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
	fr := features.BuildProjectRouterForTest("centrum", features.BuilderDeps{Pool: pool, Cache: cli}, "sports")
	return &harness{router: fr, adminPool: adminA, rdb: rdb, schemaA: schemaA, schemaB: schemaB}
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

func TestMembers_CRUD(t *testing.T) {
	h := setup(t)
	resp := doReq(t, h.router, "POST", "/members", "tok-mem-a",
		`{"name":"Budi","phone":"+6281","balance":100000}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	id, _ := m["id"].(string)
	// Top up.
	resp2 := doReq(t, h.router, "POST", "/members/"+id+"/topup", "tok-mem-a", `{"amount":50000}`)
	if resp2.StatusCode != 200 {
		t.Fatalf("topup status=%d body=%s", resp2.StatusCode, readAll(t, resp2))
	}
	m2 := readJSON(t, resp2)
	if m2["balance"] != float64(150000) {
		t.Errorf("balance=%v want 150000", m2["balance"])
	}
	// List.
	resp3 := doReq(t, h.router, "GET", "/members", "tok-mem-a", "")
	m3 := readJSON(t, resp3)
	items, _ := m3["items"].([]any)
	if len(items) != 1 {
		t.Errorf("items=%d want 1", len(items))
	}
}

func TestMembers_TopupValidation(t *testing.T) {
	h := setup(t)
	resp := doReq(t, h.router, "POST", "/members", "tok-mem-a", `{"name":"Budi"}`)
	m := readJSON(t, resp)
	id, _ := m["id"].(string)
	// Zero/negative topup → 400.
	resp2 := doReq(t, h.router, "POST", "/members/"+id+"/topup", "tok-mem-a", `{"amount":0}`)
	if resp2.StatusCode != 400 {
		t.Errorf("zero topup status=%d want 400", resp2.StatusCode)
	}
}

// TestMembers_TopupIdempotency: same Idempotency-Key twice → credited once.
// Second call returns the same balance with deduplicated:true, and exactly
// one ledger row exists.
func TestMembers_TopupIdempotency(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp := doReq(t, h.router, "POST", "/members", "tok-mem-a", `{"name":"Siti","balance":10000}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	id, _ := m["id"].(string)

	resp2 := doReqKey(t, h.router, "POST", "/members/"+id+"/topup", "tok-mem-a", `{"amount":5000}`, "top-key-1")
	if resp2.StatusCode != 200 {
		t.Fatalf("first topup status=%d body=%s", resp2.StatusCode, readAll(t, resp2))
	}
	resp3 := doReqKey(t, h.router, "POST", "/members/"+id+"/topup", "tok-mem-a", `{"amount":5000}`, "top-key-1")
	if resp3.StatusCode != 200 {
		t.Fatalf("replay status=%d want 200, body=%s", resp3.StatusCode, readAll(t, resp3))
	}
	m3 := readJSON(t, resp3)
	if m3["balance"] != float64(15000) || m3["deduplicated"] != true {
		t.Errorf("replay = %v, want balance 15000 with deduplicated:true", m3)
	}
	var n int
	if err := h.adminPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM sports_member_topups WHERE tenant_id='mem-a'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("ledger rows = %d, want 1 (no double credit)", n)
	}
}

// TestMembers_TopupIdempotencyScopedToMember: the same key on two
// DIFFERENT members credits independently. A key is (tenant, key, member),
// never just (tenant, key) — otherwise B's check returns A's balance.
func TestMembers_TopupIdempotencyScopedToMember(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	mk := func(name string, balance int) string {
		resp := doReq(t, h.router, "POST", "/members", "tok-mem-a",
			fmt.Sprintf(`{"name":%q,"balance":%d}`, name, balance))
		if resp.StatusCode != 201 {
			t.Fatalf("create %s status=%d body=%s", name, resp.StatusCode, readAll(t, resp))
		}
		m := readJSON(t, resp)
		id, _ := m["id"].(string)
		return id
	}
	idA, idB := mk("Ana", 10000), mk("Beni", 20000)

	top := func(id, key string) (int, map[string]any) {
		resp := doReqKey(t, h.router, "POST", "/members/"+id+"/topup", "tok-mem-a", `{"amount":5000}`, key)
		m := readJSON(t, resp)
		return resp.StatusCode, m
	}
	if code, _ := top(idA, "shared-key"); code != 200 {
		t.Fatalf("A topup status=%d want 200", code)
	}
	code, m := top(idB, "shared-key")
	if code != 200 {
		t.Fatalf("B topup status=%d want 200", code)
	}
	if m["balance"] != float64(25000) || m["deduplicated"] == true {
		t.Errorf("B topup = %v, want fresh credit to 25000 (not A's balance, not deduplicated)", m)
	}
	var n int
	if err := h.adminPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM sports_member_topups WHERE tenant_id='mem-a'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("ledger rows = %d, want 2 (one per member)", n)
	}
}

// TestMembers_TombstoneDelete: owner delete hides but preserves the row
// (balance intact) with who/when. Re-delete → 404.
func TestMembers_TombstoneDelete(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp := doReq(t, h.router, "POST", "/members", "tok-mem-a", `{"name":"Hapus","balance":50000}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	id, _ := readJSON(t, resp)["id"].(string)

	respD := doReq(t, h.router, "DELETE", "/members/"+id, "tok-mem-a", "")
	if respD.StatusCode != 200 {
		t.Fatalf("delete status=%d body=%s", respD.StatusCode, readAll(t, respD))
	}
	readAll(t, respD)
	if r := doReq(t, h.router, "GET", "/members/"+id, "tok-mem-a", ""); r.StatusCode != 404 {
		t.Errorf("get deleted status=%d want 404", r.StatusCode)
		readAll(t, r)
	}
	mL := readJSON(t, doReq(t, h.router, "GET", "/members", "tok-mem-a", ""))
	for _, it := range mL["items"].([]any) {
		if m, _ := it.(map[string]any); m["id"] == id {
			t.Error("deleted member still listed")
		}
	}
	var bal float64
	var delAt time.Time
	if err := h.adminPool.QueryRow(ctx,
		`SELECT balance, deleted_at FROM sports_members WHERE id=$1::uuid`, id).Scan(&bal, &delAt); err != nil {
		t.Fatalf("tombstone row missing: %v", err)
	}
	if bal != 50000 || delAt.IsZero() {
		t.Errorf("tombstone balance=%v deleted_at=%v, want 50000 + set", bal, delAt)
	}
	if r := doReq(t, h.router, "DELETE", "/members/"+id, "tok-mem-a", ""); r.StatusCode != 404 {
		t.Errorf("re-delete status=%d want 404", r.StatusCode)
		readAll(t, r)
	}
	// Topup on a deleted member → 404, not a silent credit.
	if r := doReq(t, h.router, "POST", "/members/"+id+"/topup", "tok-mem-a", `{"amount":1000}`); r.StatusCode != 404 {
		t.Errorf("topup deleted status=%d want 404", r.StatusCode)
		readAll(t, r)
	}
}

func TestMembers_TenantIsolation(t *testing.T) {
	h := setup(t)
	resp := doReq(t, h.router, "POST", "/members", "tok-mem-a", `{"name":"A Member"}`)
	m := readJSON(t, resp)
	id, _ := m["id"].(string)
	respL := doReq(t, h.router, "GET", "/members", "tok-mem-b", "")
	mL := readJSON(t, respL)
	items, _ := mL["items"].([]any)
	if len(items) != 0 {
		t.Errorf("B saw %d members, want 0", len(items))
	}
	respG := doReq(t, h.router, "GET", "/members/"+id, "tok-mem-b", "")
	if respG.StatusCode != 404 {
		t.Errorf("B get status=%d want 404", respG.StatusCode)
	}
	respT := doReq(t, h.router, "POST", "/members/"+id+"/topup", "tok-mem-b", `{"amount":1000}`)
	if respT.StatusCode != 404 {
		t.Errorf("B topup status=%d want 404", respT.StatusCode)
	}
}
