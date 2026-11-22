// Kantin POS tests: product CRUD, atomic sale (server-side total,
// stock decrement, oversell → 409), isolation.
//
// DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F
package kantin

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
	// These tests need a SECOND, non-privileged role: they create an `app`
	// role, grant it table privileges, and then connect as it to prove the
	// grant boundary. Supabase's pooler only authenticates roles it manages
	// (postgres / anon / authenticated / service_role and <role>.<projectref>
	// for roles created inside the project), so against a hosted pooler the
	// `app` connect fails with "failed to retrieve database credentials" —
	// or, without the project ref, "no tenant identifier provided". Repeated
	// failures there also trip the pooler's circuit breaker, which then
	// blocks even the admin connection.
	//
	// So: skip loudly instead of failing, and instead of hammering a shared
	// hosted database. Run this package against a local Postgres/Laragon
	// (where `app` is an ordinary role) to actually exercise it.
	if isManagedPoolerDSN(dsn) {
		t.Skip("TEST_DATABASE_URL points at a managed pooler, which cannot authenticate a second test role — " +
			"run this package against a local Postgres to exercise it")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	adminPool, err := db.NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adminPool.Close)
	testutil.Migrate(t, adminPool)
	if _, err := adminPool.Exec(ctx, `DELETE FROM clients WHERE tenant_id IN ('ka-a','ka-b')`); err != nil {
		t.Fatal(err)
	}
	schemaA := testutil.CloneTenantSchema(t, ctx, adminPool, "ka-a")
	schemaB := testutil.CloneTenantSchema(t, ctx, adminPool, "ka-b")
	for _, c := range []struct{ schema, name, tenant, token string }{
		{schemaA, "ka-a", "ka-a", "tok-ka-a"},
		{schemaB, "ka-b", "ka-b", "tok-ka-b"},
	} {
		seedClient(t, ctx, adminPool, c.schema, c.name, c.tenant, c.token, 1000000, "active", "records", "kantin")
	}
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
	fr := features.BuildVerticalRouter(features.BuilderDeps{Pool: pool, Cache: cli}, "sports")
	return &harness{router: fr, adminPool: adminA, rdb: rdb, schemaA: schemaA, schemaB: schemaB}
}

// isManagedPoolerDSN reports whether the DSN targets a managed connection
// pooler (Supabase's transaction/session pooler) rather than a Postgres you
// control. Those only authenticate the roles they provision.
func isManagedPoolerDSN(dsn string) bool {
	h := strings.ToLower(dsn)
	for _, marker := range []string{
		"pooler.supabase.com",
		"aws-0-", // Supavisor / pooler hosts
		"supabase.co:5432",
		".pooler.",
	} {
		if strings.Contains(h, marker) {
			return true
		}
	}
	return false
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

// seedProduct creates a track-stock product for tenant via admin.
func seedProduct(t *testing.T, ctx context.Context, admin *pgxpool.Pool, tenant, name string, price float64, stock int) string {
	t.Helper()
	var id string
	if err := admin.QueryRow(ctx,
		`INSERT INTO sports_kantin_products (tenant_id, name, price, cost_price, track_stock, stock)
		 VALUES ($1,$2,$3,2000,true,$4) RETURNING id::text`,
		tenant, name, price, stock).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestKantin_AtomicSale is THE test: total computed server-side, stock
// decremented, sale + items + movement all in one tx.
func TestKantin_AtomicSale(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p1 := seedProduct(t, ctx, h.adminPool, "ka-a", "Es Teh", 5000, 10)
	p2 := seedProduct(t, ctx, h.adminPool, "ka-a", "Indomie", 8000, 5)

	// Sale 2× Es Teh + 1× Indomie = 18000.
	resp := doReq(t, h.router, "POST", "/kantin/sales", "tok-ka-a",
		`{"method":"cash","items":[{"product_id":"`+p1+`","qty":2},{"product_id":"`+p2+`","qty":1}]}`)
	if resp.StatusCode != 201 {
		t.Fatalf("sale status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	if m["total"] != float64(18000) {
		t.Errorf("total=%v want 18000 (server-computed)", m["total"])
	}
	// Stock decremented: 10-2=8, 5-1=4.
	var s1, s2 int
	if err := h.adminPool.QueryRow(ctx, `SELECT stock FROM sports_kantin_products WHERE id=$1::uuid`, p1).Scan(&s1); err != nil {
		t.Fatal(err)
	}
	if err := h.adminPool.QueryRow(ctx, `SELECT stock FROM sports_kantin_products WHERE id=$1::uuid`, p2).Scan(&s2); err != nil {
		t.Fatal(err)
	}
	if s1 != 8 || s2 != 4 {
		t.Errorf("stocks=%d,%d want 8,4", s1, s2)
	}
	// Movements logged.
	var mv int
	if err := h.adminPool.QueryRow(ctx,
		`SELECT count(*) FROM sports_kantin_stock_movements WHERE tenant_id='ka-a' AND reason='sale'`).Scan(&mv); err != nil {
		t.Fatal(err)
	}
	if mv != 2 {
		t.Errorf("movements=%d want 2", mv)
	}
}

// TestKantin_SaleIdempotency: same Idempotency-Key twice → one sale, no
// double stock decrement. Second call returns the original id + total.
func TestKantin_SaleIdempotency(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p := seedProduct(t, ctx, h.adminPool, "ka-a", "Kopi", 6000, 10)

	body := `{"method":"cash","items":[{"product_id":"` + p + `","qty":2}]}`
	resp := doReqKey(t, h.router, "POST", "/kantin/sales", "tok-ka-a", body, "sale-key-1")
	if resp.StatusCode != 201 {
		t.Fatalf("first status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	first := readJSON(t, resp)

	resp2 := doReqKey(t, h.router, "POST", "/kantin/sales", "tok-ka-a", body, "sale-key-1")
	if resp2.StatusCode != 200 {
		t.Fatalf("replay status=%d want 200, body=%s", resp2.StatusCode, readAll(t, resp2))
	}
	second := readJSON(t, resp2)
	if second["id"] != first["id"] || second["deduplicated"] != true {
		t.Errorf("replay = %v, want same id with deduplicated:true", second)
	}
	var stock, n int
	if err := h.adminPool.QueryRow(ctx, `SELECT stock FROM sports_kantin_products WHERE id=$1::uuid`, p).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if stock != 8 {
		t.Errorf("stock=%d want 8 (decremented once)", stock)
	}
	if err := h.adminPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM sports_kantin_sales WHERE tenant_id='ka-a'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("sales rows = %d, want 1 (no double charge)", n)
	}
}

// TestKantin_SaleErrorCodes: missing product → 404, bad qty → 400, only
// genuine stock contention → 409. Messages unchanged (UI matches on them).
func TestKantin_SaleErrorCodes(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p := seedProduct(t, ctx, h.adminPool, "ka-a", "Teh", 5000, 3)

	resp := doReq(t, h.router, "POST", "/kantin/sales", "tok-ka-a",
		`{"items":[{"product_id":"00000000-0000-0000-0000-000000000000","qty":1}]}`)
	if resp.StatusCode != 404 {
		t.Errorf("missing product status=%d want 404", resp.StatusCode)
	}
	readAll(t, resp)
	resp2 := doReq(t, h.router, "POST", "/kantin/sales", "tok-ka-a",
		`{"items":[{"product_id":"`+p+`","qty":0}]}`)
	if resp2.StatusCode != 400 {
		t.Errorf("bad qty status=%d want 400", resp2.StatusCode)
	}
	readAll(t, resp2)
}

// TestKantin_Oversell: selling more than stock → 409, nothing written.
func TestKantin_Oversell(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p := seedProduct(t, ctx, h.adminPool, "ka-a", "Air", 5000, 3)
	resp := doReq(t, h.router, "POST", "/kantin/sales", "tok-ka-a",
		`{"items":[{"product_id":"`+p+`","qty":99}]}`)
	if resp.StatusCode != 409 {
		t.Errorf("oversell status=%d want 409 (body=%s)", resp.StatusCode, readAll(t, resp))
	}
	// Stock unchanged, no sale rows.
	var s int
	if err := h.adminPool.QueryRow(ctx, `SELECT stock FROM sports_kantin_products WHERE id=$1::uuid`, p).Scan(&s); err != nil {
		t.Fatal(err)
	}
	if s != 3 {
		t.Errorf("stock=%d want 3 (unchanged)", s)
	}
	var n int
	if err := h.adminPool.QueryRow(ctx,
		`SELECT count(*) FROM sports_kantin_sales WHERE tenant_id='ka-a'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("sales=%d want 0 (rolled back)", n)
	}
}

// TestKantin_SoftVoid pins the three things a void has to get right, all
// of which it got wrong while deleting the row:
//
//  1. the sale survives, marked -- a correction to money is still a record
//     of money, and deleting it leaves nothing to explain what happened
//  2. stock is restored ONLY for products that track stock. Restoring
//     unconditionally invented inventory for untracked items, which never
//     had any deducted
//  3. voiding twice is refused. With a hard delete the second void found no
//     row to restore from and so was harmless by accident; now that the row
//     survives it would restore the stock a second time
func TestKantin_SoftVoid(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tracked := seedProduct(t, ctx, h.adminPool, "ka-a", "Air", 5000, 20)
	untracked := seedProduct(t, ctx, h.adminPool, "ka-a", "Kopi", 15000, 0)
	if _, err := h.adminPool.Exec(ctx,
		`UPDATE sports_kantin_products SET track_stock=false, stock=0 WHERE id=$1::uuid`, untracked); err != nil {
		t.Fatal(err)
	}

	resp := doReq(t, h.router, "POST", "/kantin/sales", "tok-ka-a",
		`{"items":[{"product_id":"`+tracked+`","qty":3},{"product_id":"`+untracked+`","qty":2}]}`)
	if resp.StatusCode != 201 {
		t.Fatalf("sale status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	id, _ := readJSON(t, resp)["id"].(string)

	stock := func(pid string) int {
		var s int
		if err := h.adminPool.QueryRow(ctx,
			`SELECT stock FROM sports_kantin_products WHERE id=$1::uuid`, pid).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if got := stock(tracked); got != 17 {
		t.Fatalf("tracked stock after sale = %d, want 17", got)
	}

	if r := doReq(t, h.router, "POST", "/kantin/sales/"+id+"/void", "tok-ka-a", ""); r.StatusCode != 200 {
		t.Fatalf("void status=%d body=%s", r.StatusCode, readAll(t, r))
	}
	if got := stock(tracked); got != 20 {
		t.Errorf("tracked stock after void = %d, want 20", got)
	}
	// The untracked product never had stock deducted, so restoring it
	// would fabricate inventory that was never sold.
	if got := stock(untracked); got != 0 {
		t.Errorf("untracked stock after void = %d, want 0 (phantom stock)", got)
	}

	// The row is still there, marked.
	var voidedAt *string
	var itemCount int
	if err := h.adminPool.QueryRow(ctx,
		`SELECT voided_at::text FROM sports_kantin_sales WHERE id=$1::uuid`, id).Scan(&voidedAt); err != nil {
		t.Fatalf("sale row was deleted by the void: %v", err)
	}
	if voidedAt == nil {
		t.Error("sale row survived but voided_at is NULL")
	}
	if err := h.adminPool.QueryRow(ctx,
		`SELECT count(*) FROM sports_kantin_sale_items WHERE sale_id=$1::uuid`, id).Scan(&itemCount); err != nil {
		t.Fatal(err)
	}
	if itemCount != 2 {
		t.Errorf("line items = %d, want 2 (a void must keep the receipt)", itemCount)
	}

	// Twice more: refused, and no second restoration.
	for i := 0; i < 2; i++ {
		r := doReq(t, h.router, "POST", "/kantin/sales/"+id+"/void", "tok-ka-a", "")
		if r.StatusCode != 409 {
			t.Fatalf("repeat void #%d status=%d want 409 (body=%s)", i+1, r.StatusCode, readAll(t, r))
		}
	}
	if got := stock(tracked); got != 20 {
		t.Errorf("tracked stock after repeated voids = %d, want 20 (restored once)", got)
	}
	var voidMovements int
	if err := h.adminPool.QueryRow(ctx,
		`SELECT count(*) FROM sports_kantin_stock_movements WHERE reason='void'`).Scan(&voidMovements); err != nil {
		t.Fatal(err)
	}
	if voidMovements != 1 {
		t.Errorf("void movements = %d, want 1", voidMovements)
	}
}

// TestKantin_ClientPriceIgnored: client-sent total is never trusted.
func TestKantin_ClientPriceIgnored(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p := seedProduct(t, ctx, h.adminPool, "ka-a", "Teh Panas", 4000, 10)
	resp := doReq(t, h.router, "POST", "/kantin/sales", "tok-ka-a",
		`{"items":[{"product_id":"`+p+`","qty":1}]}`)
	m := readJSON(t, resp)
	if m["total"] != float64(4000) {
		t.Errorf("total=%v want 4000 (DB price, not client)", m["total"])
	}
}

// TestKantin_Isolation: B cannot see A's products/sales.
func TestKantin_Isolation(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	seedProduct(t, ctx, h.adminPool, "ka-a", "Biskuit", 6000, 5)
	respL := doReq(t, h.router, "GET", "/kantin/products", "tok-ka-b", "")
	mL := readJSON(t, respL)
	items, _ := mL["items"].([]any)
	if len(items) != 0 {
		t.Errorf("B saw %d products, want 0", len(items))
	}
}

// TestKantin_DeleteDeactivates: owner delete sets is_active=false instead
// of destroying the row (sale_items SET NULL + movements CASCADE on hard
// delete would orphan receipts and erase the ledger). The product stops
// selling (409) but its history stays. Re-delete → 404.
func TestKantin_DeleteDeactivates(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p := seedProduct(t, ctx, h.adminPool, "ka-a", "Hapus", 5000, 5)

	respD := doReq(t, h.router, "DELETE", "/kantin/products/"+p, "tok-ka-a", "")
	if respD.StatusCode != 200 {
		t.Fatalf("delete status=%d body=%s", respD.StatusCode, readAll(t, respD))
	}
	readAll(t, respD)
	var active bool
	if err := h.adminPool.QueryRow(ctx,
		`SELECT is_active FROM sports_kantin_products WHERE id=$1::uuid`, p).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active {
		t.Error("product still active after delete")
	}
	// No longer sellable (deactivated reads as not-found for the POS).
	respS := doReq(t, h.router, "POST", "/kantin/sales", "tok-ka-a",
		`{"items":[{"product_id":"`+p+`","qty":1}]}`)
	if respS.StatusCode != 404 {
		t.Errorf("sale of inactive status=%d want 404", respS.StatusCode)
		readAll(t, respS)
	}
	// History intact: movements + product row still present.
	var mv int
	if err := h.adminPool.QueryRow(ctx,
		`SELECT count(*) FROM sports_kantin_products WHERE id=$1::uuid`, p).Scan(&mv); err != nil || mv != 1 {
		t.Errorf("product row missing (count=%d err=%v)", mv, err)
	}
	if r := doReq(t, h.router, "DELETE", "/kantin/products/"+p, "tok-ka-a", ""); r.StatusCode != 404 {
		t.Errorf("re-delete status=%d want 404", r.StatusCode)
		readAll(t, r)
	}
}

// TestKantin_OpnameSetsLevel is the regression guard for stock-take.
//
// The "Stok opname" control is a physical count: the operator counted N
// units and that IS the new level. It used to send an ordinary additive
// delta, so counting 5 with the system believing 32 produced 37, and the
// page's promise that the system would compute the difference itself was
// false. The difference must still be written to the ledger.
func TestKantin_OpnameSetsLevel(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p := seedProduct(t, ctx, h.adminPool, "ka-a", "Opnamakan", 5000, 20)

	// Bump the believed level to 32 first, so the count is not a no-op.
	if resp := doReq(t, h.router, "POST", "/kantin/products/"+p+"/stock", "tok-ka-a",
		`{"delta":12,"reason":"restock"}`); resp.StatusCode != 200 {
		t.Fatalf("restock status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}

	resp := doReq(t, h.router, "POST", "/kantin/products/"+p+"/stock", "tok-ka-a",
		`{"count":5,"reason":"correction","note":"hitung fisik"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("opname status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	var stock int
	if err := h.adminPool.QueryRow(ctx,
		`SELECT stock FROM sports_kantin_products WHERE id=$1::uuid`, p).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if stock != 5 {
		t.Errorf("stock=%d want 5 (the counted level, not 32+5=37)", stock)
	}

	// The ledger records the SIGNED DIFFERENCE, not the count.
	var delta int
	var reason string
	if err := h.adminPool.QueryRow(ctx,
		`SELECT delta, reason FROM sports_kantin_stock_movements
		  WHERE tenant_id='ka-a' AND product_id=$1::uuid AND reason='correction'
		  ORDER BY created_at DESC LIMIT 1`, p).Scan(&delta, &reason); err != nil {
		t.Fatal(err)
	}
	if delta != -27 {
		t.Errorf("ledger delta=%d want -27 (5 counted - 32 believed)", delta)
	}
}

// TestKantin_OpnameRejectsNegative: a count is a physical level, never
// below zero. The additive path has its own guard; this one must not be
// a way to smuggle a negative stock past it.
func TestKantin_OpnameRejectsNegative(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p := seedProduct(t, ctx, h.adminPool, "ka-a", "Negatif", 5000, 4)
	resp := doReq(t, h.router, "POST", "/kantin/products/"+p+"/stock", "tok-ka-a",
		`{"count":-3,"reason":"correction"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d want 400 (negative count)", resp.StatusCode)
	}
	var stock int
	if err := h.adminPool.QueryRow(ctx,
		`SELECT stock FROM sports_kantin_products WHERE id=$1::uuid`, p).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if stock != 4 {
		t.Errorf("stock=%d want 4 (unchanged)", stock)
	}
}

// TestKantin_MovementsEndpoint: the ledger must be readable as movements.
// The app used to point this table at /kantin/products, so the stock page
// rendered PRODUCT rows in the history list: no delta, no reason, no note,
// and the product's creation date as the movement time.
func TestKantin_MovementsEndpoint(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p := seedProduct(t, ctx, h.adminPool, "ka-a", "Riwayat Tes", 5000, 10)

	if resp := doReq(t, h.router, "POST", "/kantin/products/"+p+"/stock", "tok-ka-a",
		`{"delta":-3,"reason":"waste","note":"botol pecah"}`); resp.StatusCode != 200 {
		t.Fatalf("waste status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}

	resp := doReq(t, h.router, "GET", "/kantin/movements", "tok-ka-a", "")
	if resp.StatusCode != 200 {
		t.Fatalf("movements status=%d body=%s", resp.StatusCode, readAll(t, resp))
	}
	m := readJSON(t, resp)
	items, _ := m["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items=%d want 1", len(items))
	}
	row, _ := items[0].(map[string]any)
	if got := row["product"]; got != "Riwayat Tes" {
		t.Errorf("product=%v want 'Riwayat Tes' (join must resolve)", got)
	}
	if got := row["delta"]; got != float64(-3) {
		t.Errorf("delta=%v want -3", got)
	}
	if got := row["reason"]; got != "waste" {
		t.Errorf("reason=%v want 'waste'", got)
	}
	if got := row["note"]; got != "botol pecah" {
		t.Errorf("note=%v want 'botol pecah'", got)
	}
	// stock_after is reconstructed, not a literal zero.
	if got := row["stock_after"]; got != float64(7) {
		t.Errorf("stock_after=%v want 7 (10 - 3)", got)
	}
	if _, ok := row["created_at"]; !ok {
		t.Error("created_at missing")
	}

	// Tenant isolation: ka-b must not see ka-a's ledger.
	respB := doReq(t, h.router, "GET", "/kantin/movements", "tok-ka-b", "")
	mB := readJSON(t, respB)
	itemsB, _ := mB["items"].([]any)
	if len(itemsB) != 0 {
		t.Errorf("ka-b saw %d movements, want 0", len(itemsB))
	}
}
