// Package testutil is the shared database fixture for Centrum's feature
// packages.
//
// Every test tenant gets a real cloned schema. That removes the old
// shared-table race entirely: a test no longer DELETEs and re-INSERTs rows
// that another package is using, so the advisory lock is no longer part of
// the feature setup path. LockAndRun remains for callers that still need a
// serialized transaction while migrating a package.
package testutil

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/auth"
	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/internal/dbtest"
)

// SourceSchema is the production Centrum schema every test clone is made
// from. Feature tests must not query it directly; use CloneTenantSchema.
const SourceSchema = "project_centrum"

// testLockID serialises the few remaining setup paths that still share a
// table. Distinct from the migration lock so tests never wedge a migrate.
const testLockID = 734563

// LockAndRun runs fn inside a transaction holding the shared test lock.
func LockAndRun(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
	}()
	if _, err := tx.Exec(ctx, fmt.Sprintf(`SELECT pg_advisory_xact_lock(%d)`, testLockID)); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

var (
	migrateOnce sync.Once
	migrateErr  error
)

// Migrate applies the embedded migrations once per test binary. Every setup
// calls it; the once keeps the second through Nth test cheap.
func Migrate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	migrateOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
		defer cancel()
		migrateErr = db.MigrateAll(ctx, pool)
	})
	if migrateErr != nil {
		t.Fatal(migrateErr)
	}
}

// CloneTenantSchema returns the tenant's shared clone and empties it. The
// schema is created once per test binary, then truncated between tests; this
// keeps per-test data isolation without re-creating every table.
func CloneTenantSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenant string) string {
	t.Helper()
	schema := dbtest.SharedSchema(t, ctx, pool, SourceSchema, tenant)
	if err := dbtest.ResetSchema(ctx, pool, schema); err != nil {
		t.Fatalf("reset clone %s: %v", schema, err)
	}
	return schema
}

// SchemaPool returns an admin pool whose connections run with search_path
// set to schema. Test SQL can then use unqualified table names and be sure
// it addresses the tenant's clone, never project_centrum.
func SchemaPool(t *testing.T, ctx context.Context, dsn, schema string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET search_path TO "+pgx.Identifier{schema}.Sanitize())
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Table returns a quoted schema.table reference for test SQL.
func Table(schema, table string) string {
	return dbtest.Table(schema, table)
}

// SeedStaffSession creates a staff user in the tenant's cloned schema and a
// live X-Session-Token session in the platform directory. create_session
// resolves the clone from clients.db_schema, so the lookup takes the exact
// production path.
func SeedStaffSession(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema, tenantID, email, role, rawToken string) {
	t.Helper()
	var userID string
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		`INSERT INTO %s (tenant_id, email, password_hash, role, full_name, is_active)
		 VALUES ($1, $2, 'x', $3, 'Matrix Test', true)
		 ON CONFLICT (tenant_id, email)
		 DO UPDATE SET role = EXCLUDED.role, is_active = true
		 RETURNING id::text`, Table(schema, "sports_users")),
		tenantID, email, role).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`SELECT private.create_session($1::uuid, $2, now() + interval '1 hour')`,
		userID, auth.HashToken(rawToken)); err != nil {
		t.Fatal(err)
	}
}
