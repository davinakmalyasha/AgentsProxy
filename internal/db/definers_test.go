package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestDefiners_AsAppRuntime proves migration 053's definer path works
// for the runtime role with least privilege:
//   - private.month_tokens returns exactly one client's month total
//     (two clients seeded; A must not see B's rows).
//   - Direct INSERT still works (usage_write_runtime kept).
//   - private.mint_report / expired_reports / delete_reports round-trip.
//
// NOTE: migration 063 enables RLS on clients/usage in the migration chain,
// and migration 084 removes the blanket usage read policy again. The quota
// path uses private.month_tokens; the `rls_enable_test` now asserts that
// app_runtime cannot read the usage ledger directly.
func TestDefiners_AsAppRuntime(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	adminDSN := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
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
		 VALUES ('definer', $1, 'definer-hash', 1000000, 'active'),
		        ('definer-b', 'definer-test-b', 'definer-hash-b', 1000000, 'active')
		 ON CONFLICT (tenant_id) DO NOTHING`, tenant); err != nil {
		t.Fatal(err)
	}
	var clientID, clientB string
	if err := adminPool.QueryRow(ctx,
		`SELECT id::text FROM clients WHERE tenant_id=$1`, tenant).Scan(&clientID); err != nil {
		t.Fatal(err)
	}
	if err := adminPool.QueryRow(ctx,
		`SELECT id::text FROM clients WHERE tenant_id='definer-test-b'`).Scan(&clientB); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx,
		`DELETE FROM usage WHERE client_id=$1::uuid OR client_id=$2::uuid`, clientID, clientB); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx,
		`INSERT INTO usage (client_id, tokens_in, tokens_out, cost_usd, model, kind)
		 VALUES ($1::uuid, 100, 50, 0, 'm', 'chat'), ($1::uuid, 10, 5, 0, 'm', 'chat'),
		        ($2::uuid, 1000000, 1000000, 0, 'm', 'chat')`,
		clientID, clientB); err != nil {
		t.Fatal(err)
	}

	runtimePool, err := NewPool(ctx, toRuntimeDSN(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer runtimePool.Close()

	// month_tokens sees exactly this client's rows: 100+50+10+5,
	// never the 2M seeded on the sibling client.
	var total int
	if err := runtimePool.QueryRow(ctx,
		`SELECT private.month_tokens($1::uuid)`, clientID).Scan(&total); err != nil {
		t.Fatalf("month_tokens as app_runtime: %v", err)
	}
	if total != 165 {
		t.Errorf("month_tokens = %d, want 165 (sibling rows must not leak)", total)
	}
	// Unknown client → 0, not an error.
	if err := runtimePool.QueryRow(ctx,
		`SELECT private.month_tokens('00000000-0000-0000-0000-000000000000')`).Scan(&total); err != nil {
		t.Fatalf("month_tokens unknown client: %v", err)
	}
	if total != 0 {
		t.Errorf("month_tokens unknown = %d, want 0", total)
	}

	// INSERT for the authenticated client is allowed; the policy binds the
	// row to app.client_id.
	tx, err := runtimePool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.client_id', $1, true)`, clientID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO usage (client_id, tokens_in, tokens_out, cost_usd, model, kind)
		 VALUES ($1::uuid, 1, 1, 0, 'm', 'chat')`, clientID); err != nil {
		_ = tx.Rollback(ctx)
		t.Errorf("INSERT on usage for the authenticated client should work: %v", err)
	} else if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// A write for another client under the same GUC must be denied.
	tx, err = runtimePool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.client_id', $1, true)`, clientID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO usage (client_id, tokens_in, tokens_out, cost_usd, model, kind)
		 VALUES ($1::uuid, 1, 1, 0, 'm', 'chat')`, clientB)
	_ = tx.Rollback(ctx)
	if err == nil {
		t.Error("app_runtime inserted usage for another client; policy did not bind app.client_id")
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
	if _, err := adminPool.Exec(ctx, `DELETE FROM usage WHERE client_id=$1::uuid OR client_id=$2::uuid`, clientID, clientB); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, `DELETE FROM report_objects WHERE tenant_id=$1`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, `DELETE FROM clients WHERE tenant_id=$1 OR tenant_id='definer-test-b'`, tenant); err != nil {
		t.Fatal(err)
	}
}
