// Package testutil is a shared test helper for the sports feature
// packages. It exists because four packages (facilities, bookings,
// inventory, members) run against the SAME four tables in parallel
// test runs, and their setup DELETE+INSERT sequences race on table
// locks ("tuple concurrently updated").
//
// The fix: every setup's cleanup+seed runs inside a transaction that
// takes a shared advisory lock, so the four packages serialize their
// DB setup. Normal test execution is unaffected (the critical
// section is milliseconds).
//
// DONE: Phase 4 (sports vertical) / STANDARDS A
package testutil

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/auth"
)

// testLockID serialises test setup across the sports packages. Distinct
// from the migration lock (db package): sharing it would wedge tests
// against a concurrent migrate.
const testLockID = 734563

// LockAndRun takes a session advisory lock (shared across the sports
// packages) and runs fn inside a transaction. fn must use tx for all
// its statements.
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

// SeedStaffSession creates a staff user and a live X-Session-Token
// session for role-matrix tests. The session goes through
// private.create_session (the same definer login uses) with
// auth.HashToken, so the lookup takes the exact production path.
func SeedStaffSession(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID, email, role, rawToken string) {
	t.Helper()
	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO project_centrum.sports_users (tenant_id, email, password_hash, role, full_name, is_active)
		 VALUES ($1, $2, 'x', $3, 'Matrix Test', true)
		 ON CONFLICT (tenant_id, email)
		 DO UPDATE SET role = EXCLUDED.role, is_active = true
		 RETURNING id::text`, tenantID, email, role).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`SELECT private.create_session($1::uuid, $2, now() + interval '1 hour')`,
		userID, auth.HashToken(rawToken)); err != nil {
		t.Fatal(err)
	}
}
