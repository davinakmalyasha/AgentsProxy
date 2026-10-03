package agentstools_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
	_ "github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/tools"
)

// toolTestPool connects to the real test DB (env-gated like the rest
// of the suite) and seeds a throwaway tenant. Returns ctx, app pool
// (for tool.Run — RLS-scoped), and an admin pool (RLS-exempt seeding).
func toolTestPool(t *testing.T) (context.Context, *pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	rurl := os.Getenv("REDIS_URL")
	if dsn == "" || rurl == "" {
		t.Skip("DATABASE_URL / REDIS_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	adminDSN := os.Getenv("DATABASE_ADMIN_URL")
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
	if err := db.Migrate(ctx, adminPool); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DELETE FROM sports_bookings WHERE tenant_id=$1`,
		`DELETE FROM sports_customers WHERE tenant_id=$1`,
		`DELETE FROM sports_facilities WHERE tenant_id=$1`,
		`DELETE FROM clients WHERE tenant_id=$1`,
	} {
		if _, err := adminPool.Exec(ctx, stmt, tenant); err != nil {
			t.Fatal(err)
		}
	}
	_, err = adminPool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical)
		 VALUES ($1,$1,'hash-tool-test',1000,'active','sports')`, tenant)
	if err != nil {
		t.Fatal(err)
	}
	// Seed one facility so booking rows have a valid facility_id.
	_, err = adminPool.Exec(ctx,
		`INSERT INTO sports_facilities (tenant_id, name, type) VALUES ($1,'Test Court','badminton')`, tenant)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		adminPool.Exec(ctx, `DELETE FROM sports_bookings WHERE tenant_id=$1`, tenant)
		adminPool.Exec(ctx, `DELETE FROM sports_facilities WHERE tenant_id=$1`, tenant)
		adminPool.Exec(ctx, `DELETE FROM clients WHERE tenant_id=$1`, tenant)
	})
	return ctx, pool, adminPool
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
	if !strings.Contains(out, "Rina (0812-1111-1111): 2 visits") {
		t.Errorf("top_customers=%q want 'Rina (phone): 2 visits'", out)
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
	if _, err := admin.Exec(ctx, `DELETE FROM agent_staff_payments WHERE tenant_id=$1`, tenant); err != nil {
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

func TestSetReminder_RemovedUntilPWATransport(t *testing.T) {
	_, pool, _ := toolTestPool(t)
	if _, ok := agentstools.NewToolSetFor(pool, "centrum", "owner").Get("set_reminder"); ok {
		t.Fatal("set_reminder must stay unregistered until the PWA delivery transport lands")
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
