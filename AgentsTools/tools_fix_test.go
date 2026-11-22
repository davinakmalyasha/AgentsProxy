package agentstools_test

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/internal/dbtest"
	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
	_ "github.com/davinakmalyasha/agents-proxy/projects/centrum/agent/tools"
)

// toolTestPool connects to the real test DB (env-gated like the rest
// of the suite) and seeds a throwaway tenant. Returns ctx, app pool
// (for tool.Run — RLS-scoped), and an admin pool (RLS-exempt seeding).
func toolTestPool(t *testing.T) (context.Context, *pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	rurl := os.Getenv("TEST_REDIS_URL")
	if dsn == "" || rurl == "" {
		t.Skip("TEST_DATABASE_URL / TEST_REDIS_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)
	pool, err := db.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	rdb, err := redisx.NewClient(ctx, rurl)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rdb.Close() })
	tenant := "tool-test"
	// Admin pool for RLS-exempt seeding/cleanup.
	adminDSN := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if adminDSN == "" {
		adminDSN = dsn
	}
	adminPool, err := db.NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adminPool.Close)
	// Apply migrations here rather than relying on another package having done
	// it: Go runs this package first (capital A sorts before cmd/ and internal/),
	// so on a fresh CI database the sports_* tables do not exist yet.
	if err := db.MigrateAll(ctx, adminPool); err != nil {
		t.Fatal(err)
	}
	schema := dbtest.SharedSchema(t, ctx, adminPool, "project_centrum", tenant)
	if err := dbtest.ResetSchema(ctx, adminPool, schema); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, `DELETE FROM clients WHERE tenant_id=$1`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, db_schema)
		 VALUES ($1,$1,'hash-tool-test',1000,'active','sports',$2)`, tenant, schema); err != nil {
		t.Fatal(err)
	}
	admin := dbtest.SchemaPool(t, ctx, adminDSN, schema)
	// Seed one facility so booking rows have a valid facility_id.
	if _, err := admin.Exec(ctx,
		`INSERT INTO sports_facilities (tenant_id, name, type) VALUES ($1,'Test Court','badminton')`, tenant); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = adminPool.Exec(cctx, `DELETE FROM clients WHERE tenant_id=$1`, tenant)
	})
	return ctx, pool, admin
}

// intPtr is a tiny helper for nullable override_price seeding.
func intPtr(v int) *int { return &v }

// runTool fetches the registered tool by name rather than calling its
// constructor. Constructors live in the tenant package and are unexported;
// the registry is the only way production reaches them, so tests go through
// it too. That also proves registration happened.
func runTool(t *testing.T, pool *pgxpool.Pool, name string) agentstools.Tool {
	t.Helper()
	tool, ok := agentstools.NewToolSetFor(pool, "centrum", "owner").Get(name)
	if !ok {
		t.Fatalf("tool %q is not registered", name)
	}
	return tool
}

func TestQueryIncome_UsesOverrideAndCompleted(t *testing.T) {
	ctx, pool, admin := toolTestPool(t)
	tenant := "tool-test"
	today := time.Now().In(agentstools.WibLoc()).Format("2006-01-02")
	insert := func(name, status string, price int, override *int, start string) {
		t.Helper()
		_, err := admin.Exec(ctx,
			`INSERT INTO sports_bookings (tenant_id, facility_id, date, start_time, end_time, customer_name, status, price, override_price)
			 SELECT $1, id, $2::date, $3::time, $3::time + interval '1 hour', $4, $5, $6, $7
			   FROM sports_facilities WHERE tenant_id=$1 LIMIT 1`,
			tenant, today, start, name, status, price, override)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("Alice", "confirmed", 100, intPtr(150), "19:00") // override → 150
	insert("Bob", "completed", 80, nil, "20:00")            // completed counts, raw 80
	insert("Eve", "cancelled", 1000, nil, "21:00")          // excluded

	tomorrow := time.Now().In(agentstools.WibLoc()).AddDate(0, 0, 1).Format("2006-01-02")
	args, _ := json.Marshal(map[string]string{"from": today, "to": tomorrow})
	out := runTool(t, pool, "query_income").Run(ctx, tenant, args)
	if strings.Contains(out, "error") {
		t.Fatalf("query_income error: %s", out)
	}
	if !strings.Contains(out, "230") || !strings.Contains(out, "2") {
		t.Errorf("query_income=%q want sum 230 across 2 bookings", out)
	}
}

func TestCancelBooking_GuardsCompletedAndMissing(t *testing.T) {
	ctx, pool, admin := toolTestPool(t)
	tenant := "tool-test"
	today := time.Now().In(agentstools.WibLoc()).Format("2006-01-02")
	seed := func(status string, start string) string {
		t.Helper()
		var id string
		err := admin.QueryRow(ctx,
			`INSERT INTO sports_bookings (tenant_id, facility_id, date, start_time, end_time, customer_name, status, price)
			 SELECT $1, id, $2::date, $3::time, $3::time + interval '1 hour', 'Dummy', $4, 100
			   FROM sports_facilities WHERE tenant_id=$1 LIMIT 1
			 RETURNING id::text`,
			tenant, today, start, status).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	var out string
	var err error
	doneID := seed("completed", "09:00")
	out = runTool(t, pool, "cancel_booking").Run(ctx, tenant, json.RawMessage(`{"booking_id":"`+doneID+`"}`))
	if !strings.Contains(out, "cannot cancel") {
		t.Errorf("completed cancel=%q want 'cannot cancel'", out)
	}
	var status string
	err = admin.QueryRow(ctx, `SELECT status FROM sports_bookings WHERE id=$1`, doneID).Scan(&status)
	if err != nil {
		t.Fatal(err)
	}
	if status != "completed" {
		t.Errorf("status=%q want completed", status)
	}

	out = runTool(t, pool, "cancel_booking").Run(ctx, tenant, json.RawMessage(`{"booking_id":"00000000-0000-0000-0000-000000000000"}`))
	if !strings.Contains(out, "no booking found") {
		t.Errorf("missing cancel=%q want 'no booking found'", out)
	}

	liveID := seed("confirmed", "10:00")
	out = runTool(t, pool, "cancel_booking").Run(ctx, tenant, json.RawMessage(`{"booking_id":"`+liveID+`"}`))
	if !strings.Contains(out, "cancelled") {
		t.Errorf("valid cancel=%q want cancelled", out)
	}
	err = admin.QueryRow(ctx, `SELECT status FROM sports_bookings WHERE id=$1`, liveID).Scan(&status)
	if err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" {
		t.Errorf("status=%q want cancelled", status)
	}
}

func TestCancelBooking_PaidRequiresOfflineNote(t *testing.T) {
	ctx, pool, admin := toolTestPool(t)
	tenant := "tool-test"
	today := time.Now().In(agentstools.WibLoc()).Format("2006-01-02")
	var id string
	err := admin.QueryRow(ctx,
		`INSERT INTO sports_bookings (tenant_id, facility_id, date, start_time, end_time, customer_name, status, price)
		 SELECT $1, id, $2::date, '11:00'::time, '12:00'::time, 'Paid Guy', 'confirmed', 100
		   FROM sports_facilities WHERE tenant_id=$1 LIMIT 1
		 RETURNING id::text`,
		tenant, today).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO sports_payments (tenant_id, booking_id, amount, method) VALUES ($1,$2::uuid,100,'cash')`,
		tenant, id); err != nil {
		t.Fatal(err)
	}

	// Paid without a note must be refused: payment is offline, cancelling
	// never refunds automatically.
	out := runTool(t, pool, "cancel_booking").Run(ctx, tenant, json.RawMessage(`{"booking_id":"`+id+`"}`))
	if !strings.Contains(out, "requires a cancellation note") {
		t.Fatalf("paid cancel without note=%q want note requirement", out)
	}
	var status string
	if err := admin.QueryRow(ctx, `SELECT status FROM sports_bookings WHERE id=$1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "confirmed" {
		t.Fatalf("status=%q want confirmed (refused cancel must not write)", status)
	}

	// With the offline-cash note it cancels and records the note.
	out = runTool(t, pool, "cancel_booking").Run(ctx, tenant,
		json.RawMessage(`{"booking_id":"`+id+`","note":"cash refunded offline at the till"}`))
	if !strings.Contains(out, "note recorded") {
		t.Fatalf("paid cancel with note=%q want note recorded", out)
	}
	var notes string
	if err := admin.QueryRow(ctx, `SELECT status, notes FROM sports_bookings WHERE id=$1`, id).Scan(&status, &notes); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" || !strings.Contains(notes, "cash refunded offline") {
		t.Errorf("status=%q notes=%q want cancelled with the offline note", status, notes)
	}
}

func TestTopCustomers_JoinsByPhoneAndCountsOnce(t *testing.T) {
	ctx, pool, admin := toolTestPool(t)
	tenant := "tool-test"
	today := time.Now().In(agentstools.WibLoc()).Format("2006-01-02")
	insert := func(name, phone, status, start string) {
		t.Helper()
		_, err := admin.Exec(ctx,
			`INSERT INTO sports_bookings (tenant_id, facility_id, date, start_time, end_time, customer_name, customer_phone, status)
			 SELECT $1, id, $2::date, $3::time, $3::time + interval '1 hour', $4, $5, $6
			   FROM sports_facilities WHERE tenant_id=$1 LIMIT 1`,
			tenant, today, start, name, phone, status)
		if err != nil {
			t.Fatal(err)
		}
	}
	// "Rina" books twice under two name spellings but ONE phone.
	insert("Rina", "0812-1111-1111", "confirmed", "09:00")
	insert("Rina S.", "0812-1111-1111", "completed", "10:00")
	// A different person with a different phone.
	insert("Budi", "0812-2222-2222", "confirmed", "11:00")
	// A walk-in with no customers-table row (still has a phone).
	insert("Walkin", "0000-0000-0000", "completed", "12:00")

	// The customers table row is what names the phone in the join.
	if _, err := admin.Exec(ctx,
		`INSERT INTO sports_customers (tenant_id, name, phone) VALUES ($1,'Rina','0812-1111-1111')`,
		tenant); err != nil {
		t.Fatal(err)
	}

	args, _ := json.Marshal(map[string]int{"limit": 10})
	out := runTool(t, pool, "top_customers").Run(ctx, tenant, args)
	if strings.Contains(out, "error") {
		t.Fatalf("top_customers error: %s", out)
	}
	// Phones are canonicalized at the storage boundary (065), so the
	// display carries the +62 form even though the fixture typed 0812-.
	if !strings.Contains(out, "Rina (+6281211111111): 2 visits") {
		t.Errorf("top_customers=%q want 'Rina (+62…): 2 visits'", out)
	}
	if strings.Contains(out, "Rina S.:") {
		t.Errorf("top_customers=%q should not double-count name variants", out)
	}
}

func TestGetPricing_NoRowVsError(t *testing.T) {
	ctx, pool, _ := toolTestPool(t)
	tenant := "tool-test"
	farFuture := "2999-12-31"
	out := runTool(t, pool, "get_pricing").Run(ctx, tenant, json.RawMessage(`{"date":"`+farFuture+`"}`))
	if !strings.Contains(out, "no pricing") || strings.Contains(out, "error:") {
		t.Errorf("no-row pricing=%q want clean 'no pricing'", out)
	}
	// A malformed date is not a DB error path, but a missing rule row is
	// the only realistic "no pricing" case for an active tenant.
}

// TestFixedSchemasExecute pins the schema/body alignment repair: these
// five tools' schemas used to describe arguments their bodies never read
// (or missed the ones they required), so every agent call failed or
// answered for the wrong period. Each case runs the tool for real and
// asserts a non-error result -- and for kantin, the RIGHT period.
func TestRecordStaffPayment_Runs(t *testing.T) {
	ctx, pool, admin := toolTestPool(t)
	tenant := "tool-test"
	if _, err := admin.Exec(ctx, `DELETE FROM sports_staff_payments WHERE tenant_id=$1`, tenant); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"staff_name": "Siti", "amount": 50000})
	out := runTool(t, pool, "record_staff_payment").Run(ctx, tenant, args)
	if strings.Contains(out, "error:") {
		t.Fatalf("record_staff_payment error: %s", out)
	}
	if !strings.Contains(out, "recorded payment Siti 50000") {
		t.Errorf("record_staff_payment=%q want the recorded line", out)
	}
}

func TestTopupMember_Runs(t *testing.T) {
	ctx, pool, admin := toolTestPool(t)
	tenant := "tool-test"
	if _, err := admin.Exec(ctx, `DELETE FROM sports_members WHERE tenant_id=$1`, tenant); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := admin.QueryRow(ctx,
		`INSERT INTO sports_members (tenant_id, name, balance) VALUES ($1,'M',0) RETURNING id::text`,
		tenant).Scan(&id); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"member_id": id, "amount": 10000})
	out := runTool(t, pool, "topup_member").Run(ctx, tenant, args)
	if strings.Contains(out, "error:") {
		t.Fatalf("topup_member error: %s", out)
	}
	if !strings.Contains(out, "new balance: 10000") {
		t.Errorf("topup_member=%q want the new balance", out)
	}
}

func TestAdjustStock_Runs(t *testing.T) {
	ctx, pool, admin := toolTestPool(t)
	tenant := "tool-test"
	if _, err := admin.Exec(ctx, `DELETE FROM sports_inventory WHERE tenant_id=$1`, tenant); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := admin.QueryRow(ctx,
		`INSERT INTO sports_inventory (tenant_id, name, quantity) VALUES ($1,'Shuttle',10) RETURNING id::text`,
		tenant).Scan(&id); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"item_id": id, "delta": -2})
	out := runTool(t, pool, "adjust_stock").Run(ctx, tenant, args)
	if strings.Contains(out, "error:") {
		t.Fatalf("adjust_stock error: %s", out)
	}
	if !strings.Contains(out, "new quantity: 8") {
		t.Errorf("adjust_stock=%q want quantity 8", out)
	}
}

func TestSetReminder_RequiresRecipient(t *testing.T) {
	_, pool, _ := toolTestPool(t)
	tool, ok := agentstools.NewToolSetFor(pool, "centrum", "owner").Get("set_reminder")
	if !ok {
		t.Fatal("set_reminder is not registered")
	}
	ctx := context.Background()
	for _, args := range []string{
		`{}`,
		`{"text":"x","remind_at":"2030-01-01 10:00"}`,
		`{"text":"x","remind_at":"2030-01-01 10:00","recipient":"not-an-email"}`,
		`{"text":"x","remind_at":"not-a-date","recipient":"owner@example.id"}`,
		`{"text":"x","remind_at":"2000-01-01 10:00","recipient":"owner@example.id"}`,
	} {
		out := tool.Run(ctx, "tool-test", json.RawMessage(args))
		if !strings.HasPrefix(out, "error:") {
			t.Errorf("args=%s -> %q, want error:", args, out)
		}
	}
}

func TestKantinSummary_RespectsDate(t *testing.T) {
	ctx, pool, admin := toolTestPool(t)
	tenant := "tool-test"
	if _, err := admin.Exec(ctx, `DELETE FROM sports_kantin_sale_items WHERE tenant_id=$1`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `DELETE FROM sports_kantin_sales WHERE tenant_id=$1`, tenant); err != nil {
		t.Fatal(err)
	}
	seedSale := func(day string, total int) {
		t.Helper()
		var saleID string
		if err := admin.QueryRow(ctx,
			`INSERT INTO sports_kantin_sales (tenant_id, total, created_at)
			 VALUES ($1,$2,($3::date + time '12:00') AT TIME ZONE 'Asia/Jakarta') RETURNING id::text`,
			tenant, total, day).Scan(&saleID); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx,
			`INSERT INTO sports_kantin_sale_items (tenant_id, sale_id, product_name, qty, unit_price, unit_cost)
			 VALUES ($1,$2,'Teh',2,10000,4000)`, tenant, saleID); err != nil {
			t.Fatal(err)
		}
	}
	today := time.Now().In(agentstools.WibLoc()).Format("2006-01-02")
	yesterday := time.Now().In(agentstools.WibLoc()).AddDate(0, 0, -1).Format("2006-01-02")
	seedSale(today, 20000)
	seedSale(yesterday, 99999)
	args, _ := json.Marshal(map[string]string{"date": today})
	out := runTool(t, pool, "query_kantin_summary").Run(ctx, tenant, args)
	if strings.Contains(out, "error:") {
		t.Fatalf("query_kantin_summary error: %s", out)
	}
	if !strings.Contains(out, "20000") || strings.Contains(out, "99999") {
		t.Errorf("query_kantin_summary=%q want today's 20000, not yesterday's 99999", out)
	}
}

// TestQueryRangeSummary_SingleRPC pins the project money summary RPC:
// one booking + payment + expense + kantin sale seeded,
// the dashboard tool must report all four sources in one round trip.
// Tombstoned/voided rows and out-of-range rows must not leak in.
func TestQueryRangeSummary_SingleRPC(t *testing.T) {
	ctx, pool, admin := toolTestPool(t)
	tenant := "tool-test"
	today := time.Now().In(agentstools.WibLoc()).Format("2006-01-02")
	tomorrow := time.Now().In(agentstools.WibLoc()).AddDate(0, 0, 1).Format("2006-01-02")
	// noonWIB renders a WIB-noon timestamptz from a date param $N.
	noonWIB := func(n int) string {
		return `(($` + strconv.Itoa(n) + `::date + time '12:00') AT TIME ZONE 'Asia/Jakarta')`
	}

	var bookingID string
	if err := admin.QueryRow(ctx,
		`INSERT INTO sports_bookings (tenant_id, facility_id, date, start_time, end_time, customer_name, status, price)
		 SELECT $1, id, $2::date, '19:00'::time, '20:00'::time, 'Range', 'confirmed', 100000
		   FROM sports_facilities WHERE tenant_id=$1 LIMIT 1 RETURNING id::text`,
		tenant, today).Scan(&bookingID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO sports_payments (tenant_id, booking_id, amount, method, paid_at)
		 VALUES ($1, $2, 100000, 'cash', `+noonWIB(3)+`)`, tenant, bookingID, today); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO sports_payments (tenant_id, booking_id, amount, method, paid_at, deleted_at)
		 VALUES ($1, $2, 777000, 'cash', `+noonWIB(3)+`, now())`, tenant, bookingID, today); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO sports_expenses (tenant_id, amount, spent_at) VALUES ($1, 30000, $2::date)`,
		tenant, today); err != nil {
		t.Fatal(err)
	}
	var saleID string
	if err := admin.QueryRow(ctx,
		`INSERT INTO sports_kantin_sales (tenant_id, total, created_at)
		 VALUES ($1, 25000, `+noonWIB(2)+`) RETURNING id::text`,
		tenant, today).Scan(&saleID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO sports_kantin_sale_items (tenant_id, sale_id, product_name, qty, unit_price, unit_cost)
		 VALUES ($1,$2,'Teh',1,25000,10000)`, tenant, saleID); err != nil {
		t.Fatal(err)
	}

	args, _ := json.Marshal(map[string]string{"from": today, "to": tomorrow})
	out := runTool(t, pool, "query_range_summary").Run(ctx, tenant, args)
	if strings.Contains(out, "error:") {
		t.Fatalf("query_range_summary error: %s", out)
	}
	// Received 100000 cash + 25000 kantin = Rp125.000 (Idr, dotted);
	// expenses/net render with %.0f (no separator): 30000 / 95000.
	for _, want := range []string{"125.000", "expenses 30000", "net 95000", "payments 1", "bookings 1", "best day " + today, "19:00 x1"} {
		if !strings.Contains(out, want) {
			t.Errorf("query_range_summary=%q missing %q", out, want)
		}
	}
	if strings.Contains(out, "777.000") || strings.Contains(out, "777000") {
		t.Errorf("query_range_summary=%q leaked the tombstoned 777000 payment", out)
	}
}
