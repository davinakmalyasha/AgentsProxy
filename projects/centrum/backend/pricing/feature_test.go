// Pricing feature tests: CRUD + PriceFor day/evening logic + isolation.
//
// DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F
package pricing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/davinakmalyasha/agents-proxy/projects/centrum/backend/testutil"
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
	if err := db.Migrate(ctx, adminPool); err != nil {
		t.Fatal(err)
	}
	if err := testutil.LockAndRun(ctx, adminPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM sports_pricing_rules WHERE tenant_id IN ('pr-a','pr-b');
			 DELETE FROM clients WHERE tenant_id IN ('pr-a','pr-b');`); err != nil {
			return err
		}
		for _, c := range [][]string{
			{"pr-a", "pr-a", "tok-pr-a"},
			{"pr-b", "pr-b", "tok-pr-b"},
		} {
			if err := seedClientTx(t, ctx, tx, c[0], c[1], c[2], 1000000, "active", "records", "pricing"); err != nil {
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
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features)
		 VALUES ($1,$2,$3,$4,$5,'sports',$6)`,
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

// seedRule inserts a rule (day 20000/h, evening 40000/h, day 07-17).
func seedRule(t *testing.T, ctx context.Context, admin *pgxpool.Pool, tenant string, validFrom string) {
	t.Helper()
	if _, err := admin.Exec(ctx,
		`INSERT INTO sports_pricing_rules (tenant_id, valid_from, open_time, close_time, day_start, day_end, price_day, price_evening)
		 VALUES ($1,$2::date,'07:00','23:00','07:00','17:00',20000,40000)`,
		tenant, validFrom); err != nil {
		t.Fatal(err)
	}
}

func TestPricing_CRUD(t *testing.T) {
	h := setup(t)
	resp := doReq(t, h.router, "POST", "/pricing", "tok-pr-a",
		`{"valid_from":"2026-01-01","price_day":20000,"price_evening":40000}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	id, _ := m["id"].(string)
	// List.
	resp2 := doReq(t, h.router, "GET", "/pricing", "tok-pr-a", "")
	m2 := readJSON(t, resp2)
	items, _ := m2["items"].([]any)
	if len(items) != 1 {
		t.Errorf("items=%d want 1", len(items))
	}
	// Get one.
	resp3 := doReq(t, h.router, "GET", "/pricing/"+id, "tok-pr-a", "")
	if resp3.StatusCode != 200 {
		t.Errorf("get status=%d", resp3.StatusCode)
	}
	// Duplicate valid_from → 409.
	resp4 := doReq(t, h.router, "POST", "/pricing", "tok-pr-a",
		`{"valid_from":"2026-01-01","price_day":25000}`)
	if resp4.StatusCode != 409 {
		t.Errorf("dup valid_from status=%d want 409", resp4.StatusCode)
	}
}

// TestPricing_PatchRejectsBadTime: unparsable HH:MM fails closed with 400
// (the raw string is concatenated with ::time — garbage would 500 in SQL).
func TestPricing_PatchRejectsBadTime(t *testing.T) {
	h := setup(t)
	resp := doReq(t, h.router, "POST", "/pricing", "tok-pr-a",
		`{"valid_from":"2026-01-01","price_day":20000,"price_evening":40000}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	id, _ := readJSON(t, resp)["id"].(string)
	resp2 := doReq(t, h.router, "PATCH", "/pricing/"+id, "tok-pr-a", `{"day_end":"tomorrow"}`)
	if resp2.StatusCode != 400 {
		t.Errorf("bad time status=%d want 400", resp2.StatusCode)
	}
	readAll(t, resp2)
}

func TestPricing_PriceForDayAndEvening(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	seedRule(t, ctx, h.adminPool, "pr-a", "2026-01-01")

	pool, err := db.NewPool(ctx, toRuntimeDSN(os.Getenv("DATABASE_URL")))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	date, _ := time.Parse("2006-01-02", "2026-06-10")
	// Day booking 10:00-12:00 (2h × 20000 = 40000).
	var price float64
	err = records.WithTenantTx(ctx, pool, "pr-a", func(tx pgx.Tx) error {
		var e error
		price, e = PriceFor(ctx, tx, "pr-a", date,
			mustTime("10:00"), mustTime("12:00"))
		return e
	})
	if err != nil {
		t.Fatalf("PriceFor day: %v", err)
	}
	if price != 40000 {
		t.Errorf("day price=%v want 40000 (2h×20000)", price)
	}
	// Evening booking 18:00-20:00 (2h × 40000 = 80000).
	err = records.WithTenantTx(ctx, pool, "pr-a", func(tx pgx.Tx) error {
		var e error
		price, e = PriceFor(ctx, tx, "pr-a", date,
			mustTime("18:00"), mustTime("20:00"))
		return e
	})
	if err != nil {
		t.Fatalf("PriceFor evening: %v", err)
	}
	if price != 80000 {
		t.Errorf("evening price=%v want 80000 (2h×40000)", price)
	}
}

func mustTime(s string) time.Time {
	t, _ := time.Parse("15:04", s)
	return t
}

// TestPricing_Isolation: B cannot see A's rules.
func TestPricing_Isolation(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	seedRule(t, ctx, h.adminPool, "pr-a", "2026-01-01")

	respL := doReq(t, h.router, "GET", "/pricing", "tok-pr-b", "")
	mL := readJSON(t, respL)
	items, _ := mL["items"].([]any)
	if len(items) != 0 {
		t.Errorf("B saw %d rules, want 0", len(items))
	}
}

// TestPricing_TombstoneDelete: DELETE tombstones instead of removing the
// row — getOne 404s, list excludes it, patch 404s, price resolution no
// longer sees it, but the row survives with deleted_at/deleted_by.
func TestPricing_TombstoneDelete(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp := doReq(t, h.router, "POST", "/pricing", "tok-pr-a",
		`{"valid_from":"2026-03-01","price_day":20000,"price_evening":40000}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	id, _ := m["id"].(string)

	// Delete keeps the 200 {"id","status":"deleted"} contract.
	delResp := doReq(t, h.router, "DELETE", "/pricing/"+id, "tok-pr-a", "")
	if delResp.StatusCode != 200 {
		t.Fatalf("delete status=%d body=%s", delResp.StatusCode, readAll(t, delResp))
	}
	dm := readJSON(t, delResp)
	if dm["id"] != id || dm["status"] != "deleted" {
		t.Errorf("delete body=%v want {id,status:deleted}", dm)
	}

	// getOne on the tombstone → 404, exactly as a hard delete read.
	if r := doReq(t, h.router, "GET", "/pricing/"+id, "tok-pr-a", ""); r.StatusCode != 404 {
		t.Errorf("get deleted status=%d want 404", r.StatusCode)
		readAll(t, r)
	}
	// Deleted rows can't be edited.
	if r := doReq(t, h.router, "PATCH", "/pricing/"+id, "tok-pr-a", `{"price_day":1}`); r.StatusCode != 404 {
		t.Errorf("patch deleted status=%d want 404", r.StatusCode)
		readAll(t, r)
	}
	// List excludes the row.
	mL := readJSON(t, doReq(t, h.router, "GET", "/pricing", "tok-pr-a", ""))
	items, _ := mL["items"].([]any)
	for _, it := range items {
		if it.(map[string]any)["id"] == id {
			t.Error("deleted rule still listed")
		}
	}

	// Price resolution no longer sees the tombstoned rule: with no other
	// rule covering the date, the booking-time lookup reports ErrNoRule
	// instead of pricing from a deleted row.
	pool, err := db.NewPool(ctx, toRuntimeDSN(os.Getenv("DATABASE_URL")))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	date, _ := time.Parse("2006-01-02", "2026-06-10")
	err = records.WithTenantTx(ctx, pool, "pr-a", func(tx pgx.Tx) error {
		_, e := PriceFor(ctx, tx, "pr-a", date, mustTime("10:00"), mustTime("11:00"))
		return e
	})
	if !errors.Is(err, ErrNoRule) {
		t.Errorf("PriceFor after delete err=%v want ErrNoRule", err)
	}

	// The row is still in the DB with who/when. Client-token callers
	// carry no staff UserID, so deleted_by is "" — the caller identity,
	// exactly as the payments tombstone stores it.
	var delBy string
	var delAt time.Time
	if err := h.adminPool.QueryRow(ctx,
		`SELECT deleted_by, deleted_at FROM sports_pricing_rules WHERE id=$1::uuid`, id).Scan(&delBy, &delAt); err != nil {
		t.Fatalf("tombstone row missing: %v", err)
	}
	if delAt.IsZero() {
		t.Error("deleted_at not set")
	}
	if delBy != "" {
		t.Errorf("deleted_by=%q want caller identity (empty for client token)", delBy)
	}

	// Second delete finds no live row → 404.
	if r := doReq(t, h.router, "DELETE", "/pricing/"+id, "tok-pr-a", ""); r.StatusCode != 404 {
		t.Errorf("re-delete status=%d want 404", r.StatusCode)
		readAll(t, r)
	}
}
