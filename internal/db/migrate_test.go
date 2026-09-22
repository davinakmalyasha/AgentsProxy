package db

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestMigrate_Real(t *testing.T) {
	// Migrations need CREATE EXTENSION / ALTER TABLE — use the admin
	// DSN if set, else fall back to the runtime DSN.
	dsn := os.Getenv("DATABASE_ADMIN_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
	}
	// The destructive wipe races with every other test that uses
	// the same database. Skip by default; opt in by setting
	// MIGRATE_TEST_DESTRUCTIVE=true when you want a full fresh-DB
	// run (e.g. in a dedicated CI job).
	if os.Getenv("MIGRATE_TEST_DESTRUCTIVE") != "true" {
		t.Skip("MIGRATE_TEST_DESTRUCTIVE not set; skipping destructive wipe (run alone with MIGRATE_TEST_DESTRUCTIVE=true)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	// Wipe state so we can verify migrations run cleanly on a fresh DB.
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS usage CASCADE; DROP TABLE IF EXISTS clients CASCADE; DROP TABLE IF EXISTS schema_migrations CASCADE;`); err != nil {
		t.Fatalf("wipe: %v", err)
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// Run again — must be idempotent.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate (second): %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM clients WHERE tenant_id = 'dev'`).Scan(&n); err != nil {
		t.Fatalf("query clients: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 dev client, got %d", n)
	}

	var migrations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&migrations); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if migrations < 2 {
		t.Fatalf("expected at least 2 migrations recorded, got %d", migrations)
	}
}
