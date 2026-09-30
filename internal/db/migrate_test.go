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
	// 120s, not 30s: against a remote Nano over a pooler, the first
	// migrate is ~30s of DDL round-trips alone (verified 2026-09-30:
	// 33 files applied clean, then the second run starved). Local runs
	// finish in seconds; the headroom only matters remotely.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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

	// 003 no longer seeds a dev client (Phase 1.1: the published
	// dev-token moved to scripts/seed-dev.mjs). Prove the clients write
	// path directly with a probe row instead of depending on seed data.
	// The test wipes first (line 36), so this is self-contained.
	if _, err := pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status)
		 VALUES ('probe','probe','hash-probe',1,'active')
		 ON CONFLICT (tenant_id) DO NOTHING`); err != nil {
		t.Fatalf("probe insert: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM clients WHERE tenant_id='probe'`).Scan(&n); err != nil {
		t.Fatalf("query clients: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 probe client, got %d", n)
	}

	var migrations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&migrations); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if migrations < 2 {
		t.Fatalf("expected at least 2 migrations recorded, got %d", migrations)
	}
}
