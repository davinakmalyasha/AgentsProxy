package centrumbadminton

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/projects/centrum/backend/testutil"
)

// TestLoadRangeSummaryUsesTenantSchema proves the project money function runs
// as app_runtime inside tenantdb.WithTx: RLS applies, and a cloned schema is
// read instead of project_centrum.
func TestLoadRangeSummaryUsesTenantSchema(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping money DB test")
	}
	adminDSN := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if adminDSN == "" {
		adminDSN = dsn
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	admin, err := db.NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	testutil.Migrate(t, admin)

	const tenant = "money-prov"
	cleanup := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		_, _ = admin.Exec(cctx, `DELETE FROM public.clients WHERE tenant_id=$1`, tenant)
	}
	cleanup()
	t.Cleanup(cleanup)

	schema := testutil.CloneTenantSchema(t, ctx, admin, tenant)
	if _, err := admin.Exec(ctx,
		`INSERT INTO public.clients
		   (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
		 VALUES ('money test', $1, 'hash-money-prov', 1000, 'active', 'sports', ARRAY['records'], $2)`,
		tenant, schema); err != nil {
		t.Fatal(err)
	}

	var facilityID, bookingID string
	if err := admin.QueryRow(ctx, fmt.Sprintf(
		`INSERT INTO %s (tenant_id, name, type, price_per_hour)
		 VALUES ($1, 'Court M', 'badminton', 50000) RETURNING id::text`,
		testutil.Table(schema, "sports_facilities")), tenant).Scan(&facilityID); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, fmt.Sprintf(
		`INSERT INTO %s (tenant_id, facility_id, date, start_time, end_time, customer_name, status, price)
		 VALUES ($1, $2::uuid, '2026-12-01', '09:00', '10:00', 'X', 'confirmed', 50000)
		 RETURNING id::text`,
		testutil.Table(schema, "sports_bookings")), tenant, facilityID).Scan(&bookingID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s (tenant_id, booking_id, amount, method, paid_at)
		 VALUES ($1, $2::uuid, 50000, 'cash', '2026-12-01 10:00+07')`,
		testutil.Table(schema, "sports_payments")), tenant, bookingID); err != nil {
		t.Fatal(err)
	}

	runtime := testutil.RuntimePool(t, ctx, dsn)
	s, err := loadRangeSummary(ctx, runtime, tenant, "2026-12-01", "2026-12-02")
	if err != nil {
		t.Fatal(err)
	}
	if s.Cash != 50000 || s.Count != 1 {
		t.Fatalf("summary cash=%v count=%d want 50000/1", s.Cash, s.Count)
	}
	if s.Booked != 50000 || s.Bookings != 1 {
		t.Fatalf("summary booked=%v bookings=%d want 50000/1", s.Booked, s.Bookings)
	}
}
