package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestDefiners_AsAppRuntime proves migration 053's definer path works
// for the runtime role AND that the blanket usage SELECT is gone:
//   - private.month_tokens returns exactly one client's month total.
//   - A direct SELECT on usage is denied (no policy left to allow it).
//   - Direct INSERT still works (usage_write_runtime kept).
//   - private.mint_report / expired_reports / delete_reports round-trip.
func TestDefiners_AsAppRuntime(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	adminDSN := os.Getenv("DATABASE_ADMIN_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	if adminDSN == "" {
		adminDSN = dsn
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	adminPool, err := NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer adminPool.Close()
	if err := Migrate(ctx, adminPool); err != nil {
		t.Fatal(err)
	}
	if err := bootstrapAppRuntime(ctx, adminPool); err != nil {
		t.Fatal(err)
	}

	const tenant = "definer-test"
	if _, err := adminPool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status)
		 VALUES ('definer', $1, 'definer-hash', 1000000, 'active')
		 ON CONFLICT (tenant_id) DO NOTHING`, tenant); err != nil {
		t.Fatal(err)
	}
	var clientID string
	if err := adminPool.QueryRow(ctx,
		`SELECT id::text FROM clients WHERE tenant_id=$1`, tenant).Scan(&clientID); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx,
		`DELETE FROM usage WHERE client_id=$1::uuid`, clientID); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx,
		`INSERT INTO usage (client_id, tokens_in, tokens_out, cost_usd, model, kind)
		 VALUES ($1::uuid, 100, 50, 0, 'm', 'chat'), ($1::uuid, 10, 5, 0, 'm', 'chat')`,
		clientID); err != nil {
		t.Fatal(err)
	}

	runtimePool, err := NewPool(ctx, toRuntimeDSN(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer runtimePool.Close()

	// month_tokens sees exactly this client's rows: 100+50+10+5.
	var total int
	if err := runtimePool.QueryRow(ctx,
		`SELECT private.month_tokens($1::uuid)`, clientID).Scan(&total); err != nil {
		t.Fatalf("month_tokens as app_runtime: %v", err)
	}
	if total != 165 {
		t.Errorf("month_tokens = %d, want 165", total)
	}
	// Unknown client → 0, not an error.
	if err := runtimePool.QueryRow(ctx,
		`SELECT private.month_tokens('00000000-0000-0000-0000-000000000000')`).Scan(&total); err != nil {
		t.Fatalf("month_tokens unknown client: %v", err)
	}
	if total != 0 {
		t.Errorf("month_tokens unknown = %d, want 0", total)
	}

	// Blanket SELECT must be denied: usage_read_runtime is dropped.
	_, err = runtimePool.Exec(ctx, `SELECT count(*) FROM usage`)
	if err == nil {
		t.Error("direct SELECT on usage as app_runtime should be denied (policy dropped)")
	} else if !strings.Contains(err.Error(), "42501") && !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("direct SELECT: expected RLS denial, got: %v", err)
	}

	// INSERT still allowed (usage_write_runtime kept).
	if _, err := runtimePool.Exec(ctx,
		`INSERT INTO usage (client_id, tokens_in, tokens_out, cost_usd, model, kind)
		 VALUES ($1::uuid, 1, 1, 0, 'm', 'chat')`, clientID); err != nil {
		t.Errorf("INSERT on usage as app_runtime should work: %v", err)
	}

	// Report manifest round-trip through the definers.
	if _, err := adminPool.Exec(ctx, `DELETE FROM report_objects WHERE tenant_id=$1`, tenant); err != nil {
		t.Fatal(err)
	}
	var minted bool
	if err := runtimePool.QueryRow(ctx,
		`SELECT private.mint_report('defhash1', $1, 'reports/x.xlsx', 10, -5)`,
		tenant).Scan(&minted); err != nil {
		t.Fatalf("mint_report as app_runtime: %v", err)
	}
	if !minted {
		t.Error("mint_report should return true")
	}
	var gotHash, gotTenant, gotKey string
	if err := runtimePool.QueryRow(ctx,
		`SELECT token_hash, tenant_id, object_key FROM private.expired_reports(100) WHERE tenant_id=$1`,
		tenant).Scan(&gotHash, &gotTenant, &gotKey); err != nil {
		t.Fatalf("expired_reports as app_runtime: %v", err)
	}
	if gotHash != "defhash1" || gotTenant != tenant || gotKey != "reports/x.xlsx" {
		t.Errorf("expired_reports = %q %q %q", gotHash, gotTenant, gotKey)
	}
	var deleted int
	if err := runtimePool.QueryRow(ctx,
		`SELECT private.delete_reports(ARRAY['defhash1'])`).Scan(&deleted); err != nil {
		t.Fatalf("delete_reports as app_runtime: %v", err)
	}
	if deleted != 1 {
		t.Errorf("delete_reports = %d, want 1", deleted)
	}

	// Cleanup (admin bypasses RLS).
	if _, err := adminPool.Exec(ctx, `DELETE FROM usage WHERE client_id=$1::uuid`, clientID); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, `DELETE FROM report_objects WHERE tenant_id=$1`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, `DELETE FROM clients WHERE tenant_id=$1`, tenant); err != nil {
		t.Fatal(err)
	}
}
