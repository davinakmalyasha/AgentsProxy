// Users feature tests: getOne column mapping (regression: 7 columns
// scanned into 6 targets = every fetch 500d) + list + isolation.
//
// DONE: R1b / pure upgrade
package users

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
	if _, err := adminPool.Exec(ctx, `DELETE FROM clients WHERE tenant_id IN ('usr-a','usr-b')`); err != nil {
		t.Fatal(err)
	}
	schemaA := testutil.CloneTenantSchema(t, ctx, adminPool, "usr-a")
	schemaB := testutil.CloneTenantSchema(t, ctx, adminPool, "usr-b")
	for _, c := range []struct{ schema, name, tenant, token string }{
		{schemaA, "usr-a", "usr-a", "tok-usr-a"},
		{schemaB, "usr-b", "usr-b", "tok-usr-b"},
	} {
		seedClient(t, ctx, adminPool, c.schema, c.name, c.tenant, c.token, 1000000, "active", "records", "users")
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
	fr := features.BuildProjectRouterForTest("centrum", features.BuilderDeps{Pool: pool, Cache: cli}, "sports")
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

func seedUser(t *testing.T, ctx context.Context, admin *pgxpool.Pool, schema, tenant, email, role, name, phone string) string {
	t.Helper()
	var id string
	if err := admin.QueryRow(ctx, fmt.Sprintf(
		`INSERT INTO %s (tenant_id, email, password_hash, role, full_name, phone)
		 VALUES ($1,$2,'test-hash',$3,$4,NULLIF($5,'')) RETURNING id::text`,
		testutil.Table(schema, "sports_users")),
		tenant, email, role, name, phone).Scan(&id); err != nil {
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
	b := make([]byte, 8192)
	n, _ := resp.Body.Read(b)
	return string(b[:n])
}

// TestUsers_GetOneReturnsFullProfile: the column/scan regression test.
// 7 columns selected must land in 7 targets; the phone (nullable) must
// come back as "" when unset, not 500 the fetch.
func TestUsers_GetOneReturnsFullProfile(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	id := seedUser(t, ctx, h.adminPool, h.schemaA, "usr-a", "owner@a.test", "owner", "Owner A", "08150001")
	phoneless := seedUser(t, ctx, h.adminPool, h.schemaA, "usr-a", "staff@a.test", "pegawai", "Staff A", "")

	resp := doReq(t, h.router, "GET", "/users/"+id, "tok-usr-a", "")
	if resp.StatusCode != 200 {
		t.Fatalf("getOne status=%d body=%s want 200", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	if m["email"] != "owner@a.test" || m["role"] != "owner" || m["full_name"] != "Owner A" || m["phone"] != "08150001" {
		t.Errorf("getOne = %v, want full profile with phone", m)
	}
	if m["is_active"] != true {
		t.Errorf("is_active = %v, want true", m["is_active"])
	}

	resp2 := doReq(t, h.router, "GET", "/users/"+phoneless, "tok-usr-a", "")
	if resp2.StatusCode != 200 {
		t.Fatalf("phoneless getOne status=%d body=%s want 200", resp2.StatusCode, readAll(t, resp2))
	}
	if m2, _ := readJSON(t, resp2)["phone"]; m2 != nil && m2 != "" {
		t.Errorf("phoneless phone = %v, want absent or empty (omitempty contract)", m2)
	}
}

// TestUsers_MalformedIDFilter400: garbage ?id fails closed with 400.
func TestUsers_MalformedIDFilter400(t *testing.T) {
	h := setup(t)
	resp := doReq(t, h.router, "GET", "/users?id=zzz", "tok-usr-a", "")
	if resp.StatusCode != 400 {
		t.Errorf("id garbage status=%d want 400", resp.StatusCode)
	}
	readAll(t, resp)
}

// TestUsers_ListAndIsolation: list works, B sees nothing of A.
func TestUsers_ListAndIsolation(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	seedUser(t, ctx, h.adminPool, h.schemaA, "usr-a", "owner@a.test", "owner", "Owner A", "")

	resp := doReq(t, h.router, "GET", "/users", "tok-usr-a", "")
	if resp.StatusCode != 200 {
		t.Fatalf("list status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	items, _ := m["items"].([]any)
	if len(items) != 1 {
		t.Errorf("items=%d want 1", len(items))
	}
	respB := doReq(t, h.router, "GET", "/users", "tok-usr-b", "")
	mB := readJSON(t, respB)
	itemsB, _ := mB["items"].([]any)
	if len(itemsB) != 0 {
		t.Errorf("B saw %d users, want 0", len(itemsB))
	}
}
