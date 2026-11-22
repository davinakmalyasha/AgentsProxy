package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestClientsUsageRLSEnabled pins migration 063: clients and usage must
// have row security ENABLED by the migration chain itself. 033 shipped
// only the policies (production had RLS enabled out-of-band), so a
// fresh database used to come up with these tables unprotected behind
// the permissive policies.
//
// Also proves the enable did not lock the runtime out: app_runtime must
// still read clients and append usage through the policies.
func TestClientsUsageRLSEnabled(t *testing.T) {
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

	// RLS enabled, not forced (owner writes must keep working).
	for _, table := range []string{"clients", "usage"} {
		var rls, force bool
		if err := adminPool.QueryRow(ctx,
			`SELECT relrowsecurity, relforcerowsecurity
			   FROM pg_class
			  WHERE relname = $1 AND relnamespace = 'public'::regnamespace`, table).
			Scan(&rls, &force); err != nil {
			t.Fatal(err)
		}
		if !rls {
			t.Errorf("%s: relrowsecurity = false, want true (migration 063)", table)
		}
		if force {
			t.Errorf("%s: relforcerowsecurity = true, want false (owner provisioning depends on it)", table)
		}
	}

	// The policies the runtime depends on are present.
	want := map[string][]string{
		"clients": {"clients_read_runtime"},
		// Reads go through private.month_tokens; 084 removes the blanket
		// read policy that 063 accidentally restored.
		"usage": {"usage_write_runtime"},
	}
	rows, err := adminPool.Query(ctx,
		`SELECT tablename, policyname FROM pg_policies WHERE tablename IN ('clients','usage')`)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]map[string]bool{}
	for rows.Next() {
		var table, policy string
		if err := rows.Scan(&table, &policy); err != nil {
			t.Fatal(err)
		}
		if have[table] == nil {
			have[table] = map[string]bool{}
		}
		have[table][policy] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for table, policies := range want {
		for _, p := range policies {
			if !have[table][p] {
				t.Errorf("missing policy %s on %s", p, table)
			}
		}
	}
	if have["usage"]["usage_read_runtime"] {
		t.Error("usage_read_runtime must be absent; all-tenant usage reads belong to private.month_tokens")
	}

	// Behavior: app_runtime reads clients and appends usage via the
	// policies (cleanup on the admin pool).
	const tenant = "rls63-test"
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cpool, err := NewPool(cctx, adminDSN)
		if err != nil {
			return
		}
		defer cpool.Close()
		cpool.Exec(cctx, `DELETE FROM usage WHERE client_id IN (SELECT id FROM clients WHERE tenant_id = $1)`, tenant)
		cpool.Exec(cctx, `DELETE FROM clients WHERE tenant_id = $1`, tenant)
	})
	if _, err := adminPool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status)
		 VALUES ('rls63', $1, 'rls63-hash', 1000000, 'active')
		 ON CONFLICT (tenant_id) DO NOTHING`, tenant); err != nil {
		t.Fatal(err)
	}

	runtimePool, err := NewPool(ctx, toRuntimeDSN(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer runtimePool.Close()

	var n int
	if err := runtimePool.QueryRow(ctx,
		`SELECT count(*) FROM clients WHERE tenant_id = $1`, tenant).Scan(&n); err != nil {
		t.Fatalf("app_runtime SELECT clients under RLS: %v", err)
	}
	if n != 1 {
		t.Errorf("app_runtime sees %d client rows, want 1 (policy should be permissive)", n)
	}
	if _, err := runtimePool.Exec(ctx,
		`INSERT INTO usage (client_id, tokens_in, tokens_out, cost_usd, model, kind)
		 SELECT id, 1, 1, 0, 'test-model', 'chat' FROM clients WHERE tenant_id = $1`, tenant); err != nil {
		t.Fatalf("app_runtime INSERT usage under RLS: %v", err)
	}
	// No SELECT policy means RLS filters every row, including the row just
	// inserted. The quota path uses private.month_tokens instead.
	var usageRows int
	if err := runtimePool.QueryRow(ctx, `SELECT count(*) FROM usage`).Scan(&usageRows); err != nil {
		t.Fatalf("app_runtime SELECT usage: %v", err)
	}
	if usageRows != 0 {
		t.Errorf("app_runtime sees %d usage rows; want 0 without usage_read_runtime", usageRows)
	}
}
