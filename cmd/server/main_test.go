package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/internal/tenanthealth"
	"github.com/davinakmalyasha/agents-proxy/internal/testsafety"
)

// TestReconcileQuarantineReplacesStaleBootFences pins the self-healing rule:
// a tenant fixed by a later deploy must not stay fenced by a previous
// instance's boot row, while an operator fence survives.
func TestReconcileQuarantineReplacesStaleBootFences(t *testing.T) {
	testsafety.RequireLocal(t)
	dsn := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if dsn == "" {
		dsn = os.Getenv("TEST_DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("TEST_DATABASE_ADMIN_URL not set — skipping integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	pool, err := db.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM tenant_quarantine WHERE tenant_id IN ('stale-boot','manual-fence','current-boot')`)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM clients WHERE tenant_id IN ('stale-boot','manual-fence','current-boot')`)
		tenanthealth.Release("stale-boot")
		tenanthealth.Release("manual-fence")
		tenanthealth.Release("current-boot")
	}
	cleanup()
	t.Cleanup(cleanup)

	if _, err := pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
		 VALUES ('Stale','stale-boot','t1',1,'active','agency','{}','public'),
		        ('Manual','manual-fence','t2',1,'active','agency','{}','public'),
		        ('Current','current-boot','t3',1,'active','agency','{}','public')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO tenant_quarantine (tenant_id, reason) VALUES
		   ('stale-boot','boot: fixed by this deploy'),
		   ('manual-fence','operator: investigating')`); err != nil {
		t.Fatal(err)
	}
	reconcileQuarantine(ctx, pool, map[string]string{
		"current-boot": "boot: entitlement: clients.features is empty",
	})

	rows, err := pool.Query(ctx, `SELECT tenant_id, reason FROM tenant_quarantine ORDER BY tenant_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var id, reason string
		if err := rows.Scan(&id, &reason); err != nil {
			t.Fatal(err)
		}
		got[id] = reason
	}
	if _, ok := got["stale-boot"]; ok {
		t.Errorf("stale boot fence survived reconcile: %v", got)
	}
	if got["manual-fence"] != "operator: investigating" {
		t.Errorf("manual fence changed: %v", got)
	}
	if got["current-boot"] == "" {
		t.Errorf("current boot fence missing: %v", got)
	}
}
