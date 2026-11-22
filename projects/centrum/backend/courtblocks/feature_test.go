// Court blocks feature tests: CRUD + isolation.
//
// DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F
package courtblocks

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
	if _, err := adminPool.Exec(ctx, `DELETE FROM clients WHERE tenant_id IN ('cb-a','cb-b')`); err != nil {
		t.Fatal(err)
	}
	schemaA := testutil.CloneTenantSchema(t, ctx, adminPool, "cb-a")
	schemaB := testutil.CloneTenantSchema(t, ctx, adminPool, "cb-b")
	for _, c := range []struct{ schema, name, tenant, token string }{
		{schemaA, "cb-a", "cb-a", "tok-cb-a"},
		{schemaB, "cb-b", "cb-b", "tok-cb-b"},
	} {
		seedClient(t, ctx, adminPool, c.schema, c.name, c.tenant, c.token, 1000000, "active", "records", "courtblocks")
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

func TestCourtBlocks_CRUD(t *testing.T) {
	h := setup(t)
	// All-courts block.
	resp := doReq(t, h.router, "POST", "/courtblocks", "tok-cb-a",
		`{"block_date":"2026-09-25","reason":"maintenance"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	id, _ := m["id"].(string)
	// List by date.
	resp2 := doReq(t, h.router, "GET", "/courtblocks?date=2026-09-25", "tok-cb-a", "")
	m2 := readJSON(t, resp2)
	items, _ := m2["items"].([]any)
	if len(items) != 1 {
		t.Errorf("items=%d want 1", len(items))
	}
	// Get.
	resp3 := doReq(t, h.router, "GET", "/courtblocks/"+id, "tok-cb-a", "")
	if resp3.StatusCode != 200 {
		t.Errorf("get status=%d", resp3.StatusCode)
	}
	// Delete.
	resp4 := doReq(t, h.router, "DELETE", "/courtblocks/"+id, "tok-cb-a", "")
	if resp4.StatusCode != 200 {
		t.Errorf("delete status=%d", resp4.StatusCode)
	}
}

func TestCourtBlocks_PerFacilityAndValidation(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var facID string
	if err := h.adminPool.QueryRow(ctx, fmt.Sprintf(
		`INSERT INTO %s (tenant_id, name, type) VALUES ('cb-a','Court 2','badminton') RETURNING id::text`,
		testutil.Table(h.schemaA, "sports_facilities")),
	).Scan(&facID); err != nil {
		t.Fatal(err)
	}
	resp := doReq(t, h.router, "POST", "/courtblocks", "tok-cb-a",
		`{"facility_id":"`+facID+`","block_date":"2026-09-26"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("per-facility block status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	// Bad date → 400.
	resp2 := doReq(t, h.router, "POST", "/courtblocks", "tok-cb-a", `{"block_date":"not-a-date"}`)
	if resp2.StatusCode != 400 {
		t.Errorf("bad date status=%d want 400", resp2.StatusCode)
	}
}

// TestCourtBlocks_Isolation: B cannot see A's blocks.
func TestCourtBlocks_Isolation(t *testing.T) {
	h := setup(t)
	doReq(t, h.router, "POST", "/courtblocks", "tok-cb-a", `{"block_date":"2026-09-25"}`).Body.Close()
	respL := doReq(t, h.router, "GET", "/courtblocks", "tok-cb-b", "")
	mL := readJSON(t, respL)
	items, _ := mL["items"].([]any)
	if len(items) != 0 {
		t.Errorf("B saw %d blocks, want 0", len(items))
	}
}
