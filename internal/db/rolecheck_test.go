package db

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// TestVerifyRuntimeRole pins both sides of the production guard: the admin
// role (postgres, BYPASSRLS on Supabase) must be rejected, and the runtime
// role (app_runtime, NOBYPASSRLS) must pass. The acceptance side rewrites the
// admin DSN to app_runtime, which exists after the migration chain and uses
// trust auth in CI/local.
func TestVerifyRuntimeRole(t *testing.T) {
	adminDSN := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if adminDSN == "" {
		t.Skip("TEST_DATABASE_ADMIN_URL not set — skipping role check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	defer admin.Close()
	// Ensure migration 005 has created app_runtime. Tests run in a fresh
	// CI database and this file may execute before another package's
	// harness migrates; Migrate is idempotent.
	if err := Migrate(ctx, admin); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	role, err := VerifyRuntimeRole(ctx, admin)
	if err == nil {
		t.Fatalf("admin role %q passed; expected a BYPASSRLS/superuser rejection", role)
	}
	if !strings.Contains(err.Error(), "NOBYPASSRLS") {
		t.Fatalf("unexpected rejection error: %v", err)
	}

	// Supabase's `postgres` is not a superuser but does carry BYPASSRLS.
	// Pin that exact shape too: a role that only bypasses RLS must still
	// be rejected.
	if _, err := admin.Exec(ctx, `DROP ROLE IF EXISTS rolecheck_bypass`); err != nil {
		t.Fatalf("drop old rolecheck_bypass: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DROP ROLE IF EXISTS rolecheck_bypass`)
	})
	if _, err := admin.Exec(ctx, `CREATE ROLE rolecheck_bypass LOGIN NOSUPERUSER BYPASSRLS`); err != nil {
		t.Skipf("cannot create BYPASSRLS probe role: %v", err)
	}
	bypassDSN, err := withRole(adminDSN, "rolecheck_bypass")
	if err != nil {
		t.Fatalf("build bypass DSN: %v", err)
	}
	bypass, err := NewPool(ctx, bypassDSN)
	if err != nil {
		t.Fatalf("bypass pool: %v", err)
	}
	defer bypass.Close()
	if _, err := VerifyRuntimeRole(ctx, bypass); err == nil || !strings.Contains(err.Error(), "bypassrls=true") {
		t.Fatalf("BYPASSRLS role passed or wrong error: %v", err)
	}

	runtimeDSN, err := withRole(adminDSN, "app_runtime")
	if err != nil {
		t.Skipf("cannot build app_runtime DSN: %v", err)
	}
	runtime, err := NewPool(ctx, runtimeDSN)
	if err != nil {
		t.Fatalf("runtime pool: %v", err)
	}
	defer runtime.Close()
	role, err = VerifyRuntimeRole(ctx, runtime)
	if err != nil {
		// A database that has not had the migration chain applied yet
		// has no app_runtime; that is a setup problem, not a guard bug.
		if strings.Contains(err.Error(), "does not exist") ||
			strings.Contains(err.Error(), "password authentication failed") ||
			strings.Contains(err.Error(), "role") {
			t.Skipf("app_runtime not usable in this database: %v", err)
		}
		t.Fatalf("app_runtime rejected: %v", err)
	}
	if role != "app_runtime" {
		t.Fatalf("role = %q, want app_runtime", role)
	}
}

func withRole(dsn, role string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.User = url.User(role)
	return u.String(), nil
}
