package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestBackupReader_SeesAllRows pins migration 062: the scheduled-backup
// login role must read every tenant's rows despite FORCE RLS. A
// NOBYPASSRLS reader would dump EMPTY tables — the one failure mode
// where a backup quietly becomes worthless. This test fails before 062
// (role absent) and fails if the role ever loses BYPASSRLS, the
// pg_read_all_data grant, or RLS changes shape.
func TestBackupReader_SeesAllRows(t *testing.T) {
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

	const tenant = "bk-test"
	// Separate cleanup pool (see reminder_drivers_test.go for why).
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cpool, err := NewPool(cctx, adminDSN)
		if err != nil {
			return
		}
		defer cpool.Close()
		cpool.Exec(cctx, `DELETE FROM tenant_records WHERE tenant_id = $1`, tenant)
		cpool.Exec(cctx, `DELETE FROM clients WHERE tenant_id = $1`, tenant)
	})

	if _, err := adminPool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status)
		 VALUES ('bkt', $1, 'bk-test-hash', 1000000, 'active')
		 ON CONFLICT (tenant_id) DO NOTHING`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx,
		`DELETE FROM tenant_records WHERE tenant_id = $1`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx,
		`INSERT INTO tenant_records (tenant_id, vertical, kind, title, status)
		 VALUES ($1, 'fnb', 'item', 'bk probe', 'active')`, tenant); err != nil {
		t.Fatal(err)
	}

	var want int
	if err := adminPool.QueryRow(ctx,
		`SELECT count(*) FROM tenant_records WHERE tenant_id = $1`, tenant).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if want < 1 {
		t.Fatal("admin sees no rows; seed failed")
	}

	backupPool, err := NewPool(ctx, toBackupDSN(dsn))
	if err != nil {
		t.Fatalf("backup_reader connect: %v (migration 062 missing?)", err)
	}
	defer backupPool.Close()
	var got int
	if err := backupPool.QueryRow(ctx,
		`SELECT count(*) FROM tenant_records WHERE tenant_id = $1`, tenant).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("backup_reader sees %d rows, admin %d — RLS hides rows from the backup (BYPASSRLS lost?)", got, want)
	}

	// The contrast that makes the equality meaningful: the runtime role
	// without a tenant GUC is fully policy-blocked on the same table.
	runtimePool, err := NewPool(ctx, toRuntimeDSN(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer runtimePool.Close()
	var blocked int
	if err := runtimePool.QueryRow(ctx,
		`SELECT count(*) FROM tenant_records`).Scan(&blocked); err != nil {
		t.Fatal(err)
	}
	if blocked != 0 {
		t.Errorf("runtime without GUC sees %d rows, want 0 (RLS policy not active?)", blocked)
	}
}

// toBackupDSN rewrites a DSN to the backup_reader login role, mirroring
// toCallerDSN. The role is passwordless; trust auth on CI and local dev
// lets it in like app_runtime.
func toBackupDSN(dsn string) string {
	i := strings.Index(dsn, "://")
	if i < 0 {
		return dsn
	}
	rest := dsn[i+3:]
	at := strings.Index(rest, "@")
	if at < 0 {
		return dsn
	}
	return dsn[:i+3] + "backup_reader" + rest[at:]
}
