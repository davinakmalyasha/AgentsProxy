// Mandatory isolation + CRUD + balance tests for the members feature.
//
// DONE: Phase 4 (sports vertical) / STANDARDS A, D, F
package members

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
			`DELETE FROM sports_members WHERE tenant_id IN ('mem-a','mem-b');
			 DELETE FROM clients WHERE tenant_id IN ('mem-a','mem-b');`); err != nil {
			return err
		}
		seedClientTx(t, ctx, tx, "mem-a", "mem-a", "tok-mem-a", 1000000, "active", "records", "members")
		seedClientTx(t, ctx, tx, "mem-b", "mem-b", "tok-mem-b", 1000000, "active", "records", "members")
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

// TestMembers_TenantIsolation — the mandatory isolation test.
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
