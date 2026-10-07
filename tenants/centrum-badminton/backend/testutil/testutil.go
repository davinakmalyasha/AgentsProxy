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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testLockID serialises test setup across the sports packages. Distinct
// from the migration lock (db package): sharing it would wedge tests
// against a concurrent migrate.
const testLockID = 734562

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
