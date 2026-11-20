package db

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"
)

// localDBHosts are the only databases this package may ever DROP. Laragon
// runs Postgres directly on loopback; docker-compose.yml names its service
// "postgres". Anything else is remote, and a remote database is not ours
// to wipe.
var localDBHosts = map[string]bool{
	"localhost": true,
	"127.0.0.1": true,
	"::1":       true,
	"postgres":  true, // docker-compose service
}

// requireLocalDB skips the test unless the DSN points at a local
// throwaway database.
//
// This exists because a developer machine used to carry the production
// DATABASE_ADMIN_URL in .env. Tests now read TEST_DATABASE_* only, but the
// host check stays as the last line of defence. The wipe below runs
// `DROP TABLE clients CASCADE`, and CASCADE destroys every foreign key
// that references clients while leaving the referencing tables and their
// rows in place — which silently strips the tenancy guarantee from
// sports_users and friends, and leaves a database that no longer matches
// what the migration chain would build from scratch. MIGRATE_TEST_DESTRUCTIVE
// is one env var away, so the host itself has to be checked.
func requireLocalDB(t *testing.T, dsn string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		// Don't echo the DSN — it carries a password.
		t.Skipf("cannot parse DSN (%v); refusing destructive wipe", err)
	}
	if host := u.Hostname(); !localDBHosts[host] {
		t.Skipf("refusing destructive wipe against non-local database host %q; "+
			"set MIGRATE_TEST_DESTRUCTIVE only for a local throwaway DB", host)
	}
}

func TestMigrate_Real(t *testing.T) {
	// Migrations need CREATE EXTENSION / ALTER TABLE — use the admin
	// DSN if set, else fall back to the runtime DSN.
	dsn := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if dsn == "" {
		dsn = os.Getenv("TEST_DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping integration test")
	}
	// The destructive wipe races with every other test that uses
	// the same database. Skip by default; opt in by setting
	// MIGRATE_TEST_DESTRUCTIVE=true when you want a full fresh-DB
	// run (e.g. in a dedicated CI job).
	if os.Getenv("MIGRATE_TEST_DESTRUCTIVE") != "true" {
		t.Skip("MIGRATE_TEST_DESTRUCTIVE not set; skipping destructive wipe (run alone with MIGRATE_TEST_DESTRUCTIVE=true)")
	}
	// Belt and braces: the opt-in flag is not authorisation to wipe a
	// shared or hosted database.
	requireLocalDB(t, dsn)
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
	// Dropping only clients/usage leaves the custom tables, functions, and
	// private schema behind; a later CREATE OR REPLACE then fails on a
	// return-type change that would never happen on a truly empty database.
	if _, err := pool.Exec(ctx, `
		DROP SCHEMA IF EXISTS public CASCADE;
		CREATE SCHEMA public;
		GRANT USAGE ON SCHEMA public TO PUBLIC;
		DROP SCHEMA IF EXISTS private CASCADE;
		DROP SCHEMA IF EXISTS project_dbtest CASCADE;
	`); err != nil {
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
