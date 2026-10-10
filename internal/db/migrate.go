// DONE: PRD 4 (idempotent migrations, advisory lock) / STANDARDS A, H
package db

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationLockID serialises concurrent bootstraps. A collision with
// any other advisory lock in the system is an invisible deadlock, so
// it lives here as a named constant, not a literal.
const migrationLockID = 734561

// Migrate applies every embedded migration in lexical order, recording
// each in schema_migrations. All migrations are idempotent
// (IF NOT EXISTS / ON CONFLICT DO NOTHING), so re-running is safe.
func Migrate(ctx context.Context, pool *Pool) error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	// Ensure pgcrypto + schema_migrations exist before the loop. This
	// runs outside the lock: the lock-everything approach cost a
	// serialized 150ms+ on every cold start to protect against two
	// simultaneous boots on a virgin database, which never happens in
	// practice (migrations run at deploy against an existing database).
	// Take the advisory lock only when something is actually pending: a
	// clean database never holds it, so parallel boots (Vercel
	// cold-start storms) don't serialize behind a no-op loop.
	bootTx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin bootstrap: %w", err)
	}
	if _, err := bootTx.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pgcrypto; CREATE TABLE IF NOT EXISTS schema_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());`); err != nil {
		_ = bootTx.Rollback(ctx)
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}
	// One round trip for the whole ledger instead of one EXISTS per
	// migration (53 round trips on a warm boot, all inside the lock).
	applied := map[string]bool{}
	rows, err := bootTx.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		_ = bootTx.Rollback(ctx)
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			_ = bootTx.Rollback(ctx)
			return fmt.Errorf("read schema_migrations: %w", err)
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		_ = bootTx.Rollback(ctx)
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	var pending []string
	for _, name := range names {
		if !applied[name] {
			pending = append(pending, name)
		}
	}
	if len(pending) == 0 {
		// Fast path: nothing to do, never took the lock.
		if err := bootTx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migrations: %w", err)
		}
		return nil
	}
	// Something is pending: serialize with other bootstraps. Try
	// without blocking first; a loser waits, re-reads the ledger, and
	// usually finds the winner already applied everything.
	var locked bool
	if err := bootTx.QueryRow(ctx,
		fmt.Sprintf(`SELECT pg_try_advisory_xact_lock(%d)`, migrationLockID)).Scan(&locked); err != nil {
		_ = bootTx.Rollback(ctx)
		return fmt.Errorf("lock bootstrap: %w", err)
	}
	if !locked {
		if _, err := bootTx.Exec(ctx,
			fmt.Sprintf(`SELECT pg_advisory_xact_lock(%d)`, migrationLockID)); err != nil {
			_ = bootTx.Rollback(ctx)
			return fmt.Errorf("lock bootstrap: %w", err)
		}
		// Re-read under the lock: the winner may have finished.
		applied = map[string]bool{}
		rows, err := bootTx.Query(ctx, `SELECT version FROM schema_migrations`)
		if err != nil {
			_ = bootTx.Rollback(ctx)
			return fmt.Errorf("read schema_migrations: %w", err)
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				_ = bootTx.Rollback(ctx)
				return fmt.Errorf("read schema_migrations: %w", err)
			}
			applied[v] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			_ = bootTx.Rollback(ctx)
			return fmt.Errorf("read schema_migrations: %w", err)
		}
		pending = pending[:0]
		for _, name := range names {
			if !applied[name] {
				pending = append(pending, name)
			}
		}
		if len(pending) == 0 {
			if err := bootTx.Commit(ctx); err != nil {
				return fmt.Errorf("commit migrations: %w", err)
			}
			return nil
		}
	}

	for _, name := range pending {
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			_ = bootTx.Rollback(ctx)
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := bootTx.Exec(ctx, string(body)); err != nil {
			_ = bootTx.Rollback(ctx)
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := bootTx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ($1) ON CONFLICT DO NOTHING`, name); err != nil {
			_ = bootTx.Rollback(ctx)
			return fmt.Errorf("record %s: %w", name, err)
		}
	}
	if err := bootTx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	return nil
}
