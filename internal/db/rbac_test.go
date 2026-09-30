package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DONE: PRD 6.5 (RBAC) / STANDARDS D, H

// TestAppRuntime_CannotMutateClients: production runtime role
// has SELECT-only on `clients`. Any INSERT/UPDATE/DELETE on that
// table must fail with SQLSTATE 42501 (insufficient privilege).
//
// This is the security boundary that stops a runaway script or a
// compromised service from self-provisioning a fat quota.
func TestAppRuntime_CannotMutateClients(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	adminDSN := os.Getenv("DATABASE_ADMIN_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	if adminDSN == "" {
		adminDSN = dsn
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	adminPool, err := NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer adminPool.Close()
	if err := bootstrapAppRuntime(ctx, adminPool); err != nil {
		t.Fatal(err)
	}

	runtimePool, err := NewPool(ctx, toRuntimeDSN(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer runtimePool.Close()

	cases := []struct {
		name string
		sql  string
	}{
		{"insert", `INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens) VALUES ('rbac', 'rbac-tenant', repeat('a',64)::text, 1)`},
		{"update", `UPDATE clients SET name = 'x' WHERE id IN (SELECT id FROM clients LIMIT 1)`},
		{"delete", `DELETE FROM clients WHERE id IN (SELECT id FROM clients LIMIT 1)`},
	}
	for _, c := range cases {
		_, err := runtimePool.Exec(ctx, c.sql)
		if err == nil {
			t.Errorf("app_runtime %s on clients should be denied, got nil err", c.name)
			continue
		}
		if !strings.Contains(err.Error(), "42501") {
			t.Errorf("app_runtime %s: expected SQLSTATE 42501, got: %v", c.name, err)
		}
	}

	// SELECT on clients MUST work (the runtime needs to look up
	// cached clients).
	_, err = runtimePool.Exec(ctx, `SELECT id FROM clients LIMIT 1`)
	if err != nil {
		t.Errorf("app_runtime SELECT on clients should work, got: %v", err)
	}
}

// TestAppRuntime_CanMutateTenantRecords: the runtime role can DML
// the business table (with RLS via the GUC).
func TestAppRuntime_CanMutateTenantRecords(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	adminDSN := os.Getenv("DATABASE_ADMIN_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	if adminDSN == "" {
		adminDSN = dsn
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	adminPool, err := NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer adminPool.Close()
	if err := bootstrapAppRuntime(ctx, adminPool); err != nil {
		t.Fatal(err)
	}
	runtimePool, err := NewPool(ctx, toRuntimeDSN(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer runtimePool.Close()

	tx, err := runtimePool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", "rbac-tenant"); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO tenant_records (tenant_id, vertical, kind, title, status) VALUES ($1, 'fnb', 'item', 'rbac-ok', 'active')`,
		"rbac-tenant",
	)
	if err != nil {
		t.Errorf("app_runtime INSERT into tenant_records should work, got: %v", err)
	}
}

// bootstrapAppRuntime mirrors 005_rbac.sql but is idempotent and
// callable from tests. See that migration for the canonical version.
func bootstrapAppRuntime(ctx context.Context, admin *pgxpool.Pool) error {
	stmts := []string{
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_runtime') THEN
				CREATE ROLE app_runtime LOGIN;
			ELSE
				ALTER ROLE app_runtime LOGIN;
			END IF;
		END $$`,
		`GRANT USAGE ON SCHEMA public TO app_runtime`,
		`GRANT SELECT ON clients TO app_runtime`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_records TO app_runtime`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON usage TO app_runtime`,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_runtime`,
	}
	for _, s := range stmts {
		if _, err := admin.Exec(ctx, s); err != nil {
			return err
		}
	}
	return nil
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
	// If DATABASE_URL is already the runtime role (standard Supabase
	// setup: user `app_runtime` or `app_runtime.<ref>`), return it
	// unchanged. The pooler routes on the full `user.ref` form and the
	// password must be preserved — the old rewrite stripped both,
	// producing an unroutable, passwordless DSN.
	userinfo := rest[:at]
	user := userinfo
	if ci := strings.Index(userinfo, ":"); ci >= 0 {
		user = userinfo[:ci]
	}
	if user == "app_runtime" || strings.HasPrefix(user, "app_runtime.") {
		return dsn
	}
	// Legacy local path (Laragon trust auth): swap the user, drop
	// credentials.
	return dsn[:i+3] + "app_runtime" + rest[at:]
}
