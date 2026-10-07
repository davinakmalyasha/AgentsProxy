// Mandatory isolation + CRUD tests for the facilities feature.
// The runtime pool connects as the non-superuser `app` role so RLS
// applies — this is how production runs and the only way to catch
// cross-tenant leaks. A custom feature without a passing isolation
// test does not ship.
//
// DONE: Phase 4 (sports vertical) / STANDARDS A, D, F
package facilities

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
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
	"github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/testutil"
)

type harness struct {
	router http.Handler
	pool   *pgxpool.Pool
	rdb    *redis.Client
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
	// Clean up this test's own tenants under an advisory lock so the
	// four sports packages don't race their DELETE+INSERT on the same
	// tables (SQLSTATE XX000 "tuple concurrently updated").
	if err := testutil.LockAndRun(ctx, adminPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM sports_facilities WHERE tenant_id IN ('fac-a','fac-b','fac-c');
			 DELETE FROM clients WHERE tenant_id IN ('fac-a','fac-b','fac-c');`); err != nil {
			return err
		}
		seedClientTx(t, ctx, tx, "fac-a", "fac-a", "tok-fac-a", 1000000, "active", "records", "facilities")
		seedClientTx(t, ctx, tx, "fac-b", "fac-b", "tok-fac-b", 1000000, "active", "records", "facilities")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pool, err := db.NewPool(ctx, toAppDSN(dsn))
	if err != nil {
		t.Fatalf("connect as app: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := adminPool.Exec(ctx,
		`DELETE FROM sports_facilities WHERE tenant_id IN ('fac-a','fac-b','fac-c');`); err != nil {
		t.Fatal(err)
	}

	rdb, err := redisx.NewClient(ctx, rurl)
	if err != nil {
		t.Fatal(err)
	}
	rdb.FlushDB(ctx)
	t.Cleanup(func() { rdb.Close() })

	cli := &cache.Client{DB: pool, RDB: rdb}
	// Build a vertical router with the facilities feature mounted,
	// exactly like the builder does for /v1/sports.
	features.ResetForTest()
	Register()
	fr := features.BuildVerticalRouter(features.BuilderDeps{Pool: pool, Cache: cli}, "sports")
	return &harness{router: fr, pool: pool, rdb: rdb}
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
	featuresList := feats
	if len(featuresList) == 0 {
		featuresList = []string{"records"}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features)
		 VALUES ($1,$2,$3,$4,$5,'sports',$6)`,
		name, tenant, hash, quota, status, featuresList,
	); err != nil {
		t.Fatal(err)
	}
}

// seedClientTx is the tx variant used inside the locked setup.
func seedClientTx(t *testing.T, ctx context.Context, tx pgx.Tx, name, tenant, token string, quota int, status string, feats ...string) {
	t.Helper()
	hash := auth.HashToken(token)
	featuresList := feats
	if len(featuresList) == 0 {
		featuresList = []string{"records"}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features)
		 VALUES ($1,$2,$3,$4,$5,'sports',$6)`,
		name, tenant, hash, quota, status, featuresList,
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

func TestFacilities_CRUD(t *testing.T) {
	h := setup(t)
	// Create
	resp := doReq(t, h.router, "POST", "/facilities", "tok-fac-a",
		`{"name":"Court 3","type":"badminton","capacity":2,"price_per_hour":50000,"open_time":"06:00","close_time":"22:00"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatal("create: no id")
	}
	// List
	resp2 := doReq(t, h.router, "GET", "/facilities", "tok-fac-a", "")
	if resp2.StatusCode != 200 {
		t.Fatalf("list status=%d", resp2.StatusCode)
	}
	m2 := readJSON(t, resp2)
	items, _ := m2["items"].([]any)
	if len(items) != 1 {
		t.Errorf("items=%d want 1", len(items))
	}
	// Get one
	resp3 := doReq(t, h.router, "GET", "/facilities/"+id, "tok-fac-a", "")
	if resp3.StatusCode != 200 {
		t.Errorf("get status=%d", resp3.StatusCode)
	}
	// Patch
	resp4 := doReq(t, h.router, "PATCH", "/facilities/"+id, "tok-fac-a", `{"price_per_hour":60000}`)
	if resp4.StatusCode != 200 {
		t.Errorf("patch status=%d", resp4.StatusCode)
	}
	// Delete
	resp5 := doReq(t, h.router, "DELETE", "/facilities/"+id, "tok-fac-a", "")
	if resp5.StatusCode != 200 {
		t.Errorf("delete status=%d", resp5.StatusCode)
	}
	// Get after delete → 404
	resp6 := doReq(t, h.router, "GET", "/facilities/"+id, "tok-fac-a", "")
	if resp6.StatusCode != 404 {
		t.Errorf("get-after-delete status=%d want 404", resp6.StatusCode)
	}
}

func TestFacilities_Validation(t *testing.T) {
	h := setup(t)
	// Missing name
	resp := doReq(t, h.router, "POST", "/facilities", "tok-fac-a", `{"type":"badminton"}`)
	if resp.StatusCode != 400 {
		t.Errorf("missing name status=%d want 400", resp.StatusCode)
	}
	// Invalid type
	resp2 := doReq(t, h.router, "POST", "/facilities", "tok-fac-a", `{"name":"x","type":"skydiving"}`)
	if resp2.StatusCode != 400 {
		t.Errorf("invalid type status=%d want 400", resp2.StatusCode)
	}
	// open after close
	resp3 := doReq(t, h.router, "POST", "/facilities", "tok-fac-a",
		`{"name":"x","open_time":"22:00","close_time":"06:00"}`)
	if resp3.StatusCode != 400 {
		t.Errorf("open>close status=%d want 400", resp3.StatusCode)
	}
}

// TestFacilities_TenantIsolation — the mandatory isolation test.
func TestFacilities_TenantIsolation(t *testing.T) {
	h := setup(t)
	resp := doReq(t, h.router, "POST", "/facilities", "tok-fac-a",
		`{"name":"A Court","type":"badminton"}`)
	m := readJSON(t, resp)
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatal("create: no id")
	}
	// B sees nothing in list.
	respL := doReq(t, h.router, "GET", "/facilities", "tok-fac-b", "")
	mL := readJSON(t, respL)
	items, _ := mL["items"].([]any)
	if len(items) != 0 {
		t.Errorf("B saw %d facilities, want 0", len(items))
	}
	// B cannot get A's by id.
	respG := doReq(t, h.router, "GET", "/facilities/"+id, "tok-fac-b", "")
	if respG.StatusCode != 404 {
		t.Errorf("B get status=%d want 404", respG.StatusCode)
	}
	// B cannot patch A's.
	respP := doReq(t, h.router, "PATCH", "/facilities/"+id, "tok-fac-b", `{"name":"hijack"}`)
	if respP.StatusCode != 404 {
		t.Errorf("B patch status=%d want 404", respP.StatusCode)
	}
	// B cannot delete A's.
	respD := doReq(t, h.router, "DELETE", "/facilities/"+id, "tok-fac-b", "")
	if respD.StatusCode != 404 {
		t.Errorf("B delete status=%d want 404", respD.StatusCode)
	}
}

// TestFacilities_Gate: a client without the facilities feature gets 403.
func TestFacilities_Gate(t *testing.T) {
	h := setup(t)
	// Seed a client withOUT facilities (default = records only).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	seedClient(t, ctx, h.pool, "fac-c", "fac-c", "tok-fac-c", 1000000, "active")
	// The gate reads the cached client; seed wrote to DB, flush to re-read.
	h.rdb.FlushDB(ctx)
	resp := doReq(t, h.router, "GET", "/facilities", "tok-fac-c", "")
	if resp.StatusCode != 403 {
		t.Errorf("gate status=%d want 403", resp.StatusCode)
	}
}

// TestFacilities_TombstoneDelete: DELETE tombstones instead of removing the
// row — getOne 404s, list excludes it, patch 404s, but the row survives
// with deleted_at/deleted_by. Uses a booking-free court so the 409 in-use
// guard does not trigger.
func TestFacilities_TombstoneDelete(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp := doReq(t, h.router, "POST", "/facilities", "tok-fac-a",
		`{"name":"Tomb Court","type":"badminton"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	id, _ := m["id"].(string)

	// Delete keeps the 200 {"id","status":"deleted"} contract.
	delResp := doReq(t, h.router, "DELETE", "/facilities/"+id, "tok-fac-a", "")
	if delResp.StatusCode != 200 {
		t.Fatalf("delete status=%d body=%s", delResp.StatusCode, readAll(t, delResp))
	}
	dm := readJSON(t, delResp)
	if dm["id"] != id || dm["status"] != "deleted" {
		t.Errorf("delete body=%v want {id,status:deleted}", dm)
	}

	// getOne on the tombstone → 404, exactly as a hard delete read.
	if r := doReq(t, h.router, "GET", "/facilities/"+id, "tok-fac-a", ""); r.StatusCode != 404 {
		t.Errorf("get deleted status=%d want 404", r.StatusCode)
		readAll(t, r)
	}
	// Deleted rows can't be edited.
	if r := doReq(t, h.router, "PATCH", "/facilities/"+id, "tok-fac-a", `{"name":"x"}`); r.StatusCode != 404 {
		t.Errorf("patch deleted status=%d want 404", r.StatusCode)
		readAll(t, r)
	}
	// List excludes the row.
	mL := readJSON(t, doReq(t, h.router, "GET", "/facilities", "tok-fac-a", ""))
	items, _ := mL["items"].([]any)
	for _, it := range items {
		if it.(map[string]any)["id"] == id {
			t.Error("deleted facility still listed")
		}
	}

	// The row is still in the DB with who/when. Client-token callers
	// carry no staff UserID, so deleted_by is "" — the caller identity,
	// exactly as the payments tombstone stores it. Read through the
	// tenant-scoped helper since this harness keeps only the app pool.
	var delBy string
	var delAt time.Time
	if err := records.WithTenantTx(ctx, h.pool, "fac-a", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT deleted_by, deleted_at FROM sports_facilities WHERE id=$1::uuid`, id).Scan(&delBy, &delAt)
	}); err != nil {
		t.Fatalf("tombstone row missing: %v", err)
	}
	if delAt.IsZero() {
		t.Error("deleted_at not set")
	}
	if delBy != "" {
		t.Errorf("deleted_by=%q want caller identity (empty for client token)", delBy)
	}

	// Second delete finds no live row → 404.
	if r := doReq(t, h.router, "DELETE", "/facilities/"+id, "tok-fac-a", ""); r.StatusCode != 404 {
		t.Errorf("re-delete status=%d want 404", r.StatusCode)
		readAll(t, r)
	}
}
