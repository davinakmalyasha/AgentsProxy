// Recurring series feature tests: CRUD + isolation.
//
// DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F
package series

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
			`DELETE FROM project_centrum.sports_recurring_series WHERE tenant_id IN ('se-a','se-b');
			 DELETE FROM project_centrum.sports_facilities WHERE tenant_id IN ('se-a','se-b');
			 DELETE FROM clients WHERE tenant_id IN ('se-a','se-b');`); err != nil {
			return err
		}
		for _, c := range [][]string{{"se-a", "se-a", "tok-se-a"}, {"se-b", "se-b", "tok-se-b"}} {
			if err := seedClientTx(t, ctx, tx, c[0], c[1], c[2], 1000000, "active", "records", "series"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO project_centrum.sports_facilities (tenant_id, name, type) VALUES ($1,'Court 1','badminton')`,
				c[1]); err != nil {
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

func facilityID(t *testing.T, ctx context.Context, admin *pgxpool.Pool, tenant string) string {
	t.Helper()
	var id string
	if err := admin.QueryRow(ctx,
		`SELECT id::text FROM project_centrum.sports_facilities WHERE tenant_id=$1 LIMIT 1`, tenant).Scan(&id); err != nil {
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
	b := make([]byte, 4096)
	n, _ := resp.Body.Read(b)
	return string(b[:n])
}

func TestSeries_CRUD(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fac := facilityID(t, ctx, h.adminPool, "se-a")
	// Create (every Sunday 19-21).
	resp := doReq(t, h.router, "POST", "/series", "tok-se-a",
		`{"facility_id":"`+fac+`","customer_name":"Komunitas A","weekday":0,"start_time":"19:00","end_time":"21:00","start_date":"2026-09-01"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	id, _ := m["id"].(string)
	// List.
	resp2 := doReq(t, h.router, "GET", "/series", "tok-se-a", "")
	m2 := readJSON(t, resp2)
	items, _ := m2["items"].([]any)
	if len(items) != 1 {
		t.Errorf("items=%d want 1", len(items))
	}
	// Patch → paused.
	resp3 := doReq(t, h.router, "PATCH", "/series/"+id, "tok-se-a", `{"status":"paused"}`)
	if resp3.StatusCode != 200 {
		t.Errorf("patch status=%d", resp3.StatusCode)
	}
	// Get → paused.
	resp4 := doReq(t, h.router, "GET", "/series/"+id, "tok-se-a", "")
	m4 := readJSON(t, resp4)
	if m4["status"] != "paused" {
		t.Errorf("status=%v want paused", m4["status"])
	}
	// Delete.
	resp5 := doReq(t, h.router, "DELETE", "/series/"+id, "tok-se-a", "")
	if resp5.StatusCode != 200 {
		t.Errorf("delete status=%d", resp5.StatusCode)
	}
}

func TestSeries_Validation(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fac := facilityID(t, ctx, h.adminPool, "se-a")
	// Bad weekday.
	resp := doReq(t, h.router, "POST", "/series", "tok-se-a",
		`{"facility_id":"`+fac+`","customer_name":"x","weekday":9,"start_time":"19:00","end_time":"21:00","start_date":"2026-09-01"}`)
	if resp.StatusCode != 400 {
		t.Errorf("bad weekday status=%d want 400", resp.StatusCode)
	}
	// end before start.
	resp2 := doReq(t, h.router, "POST", "/series", "tok-se-a",
		`{"facility_id":"`+fac+`","customer_name":"x","weekday":1,"start_time":"21:00","end_time":"19:00","start_date":"2026-09-01"}`)
	if resp2.StatusCode != 400 {
		t.Errorf("end<start status=%d want 400", resp2.StatusCode)
	}
}

// TestSeries_Isolation: B cannot see A's series.
func TestSeries_Isolation(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fac := facilityID(t, ctx, h.adminPool, "se-a")
	doReq(t, h.router, "POST", "/series", "tok-se-a",
		`{"facility_id":"`+fac+`","customer_name":"A","weekday":2,"start_time":"19:00","end_time":"21:00","start_date":"2026-09-01"}`).Body.Close()
	respL := doReq(t, h.router, "GET", "/series", "tok-se-b", "")
	mL := readJSON(t, respL)
	items, _ := mL["items"].([]any)
	if len(items) != 0 {
		t.Errorf("B saw %d series, want 0", len(items))
	}
}

// TestSeries_TombstoneDelete: DELETE tombstones instead of removing the
// row — getOne 404s, list excludes it, patch 404s, but the row survives
// with deleted_at/deleted_by.
func TestSeries_TombstoneDelete(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fac := facilityID(t, ctx, h.adminPool, "se-a")

	resp := doReq(t, h.router, "POST", "/series", "tok-se-a",
		`{"facility_id":"`+fac+`","customer_name":"Tomb Komunitas","weekday":0,"start_time":"19:00","end_time":"21:00","start_date":"2026-09-01"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	id, _ := m["id"].(string)

	// Delete keeps the 200 {"id","status":"deleted"} contract.
	delResp := doReq(t, h.router, "DELETE", "/series/"+id, "tok-se-a", "")
	if delResp.StatusCode != 200 {
		t.Fatalf("delete status=%d body=%s", delResp.StatusCode, readAll(t, delResp))
	}
	dm := readJSON(t, delResp)
	if dm["id"] != id || dm["status"] != "deleted" {
		t.Errorf("delete body=%v want {id,status:deleted}", dm)
	}

	// getOne on the tombstone → 404, exactly as a hard delete read.
	if r := doReq(t, h.router, "GET", "/series/"+id, "tok-se-a", ""); r.StatusCode != 404 {
		t.Errorf("get deleted status=%d want 404", r.StatusCode)
		readAll(t, r)
	}
	// Deleted rows can't be edited.
	if r := doReq(t, h.router, "PATCH", "/series/"+id, "tok-se-a", `{"status":"paused"}`); r.StatusCode != 404 {
		t.Errorf("patch deleted status=%d want 404", r.StatusCode)
		readAll(t, r)
	}
	// List excludes the row.
	mL := readJSON(t, doReq(t, h.router, "GET", "/series", "tok-se-a", ""))
	items, _ := mL["items"].([]any)
	for _, it := range items {
		if it.(map[string]any)["id"] == id {
			t.Error("deleted series still listed")
		}
	}

	// The row is still in the DB with who/when. Client-token callers
	// carry no staff UserID, so deleted_by is "" — the caller identity,
	// exactly as the payments tombstone stores it.
	var delBy string
	var delAt time.Time
	if err := h.adminPool.QueryRow(ctx,
		`SELECT deleted_by, deleted_at FROM project_centrum.sports_recurring_series WHERE id=$1::uuid`, id).Scan(&delBy, &delAt); err != nil {
		t.Fatalf("tombstone row missing: %v", err)
	}
	if delAt.IsZero() {
		t.Error("deleted_at not set")
	}
	if delBy != "" {
		t.Errorf("deleted_by=%q want caller identity (empty for client token)", delBy)
	}

	// Second delete finds no live row → 404.
	if r := doReq(t, h.router, "DELETE", "/series/"+id, "tok-se-a", ""); r.StatusCode != 404 {
		t.Errorf("re-delete status=%d want 404", r.StatusCode)
		readAll(t, r)
	}
}
