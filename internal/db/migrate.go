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

	// Ensure pgcrypto + schema_migrations exist before the loop. Wrap
	// in a tx with the same advisory lock as the per-migration step so
	// concurrent first-time bootstraps don't race on CREATE EXTENSION.
	bootTx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin bootstrap: %w", err)
	}
	if _, err := bootTx.Exec(ctx, `SELECT pg_advisory_xact_lock(734561)`); err != nil {
		_ = bootTx.Rollback(ctx)
		return fmt.Errorf("lock bootstrap: %w", err)
	}
	if _, err := bootTx.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pgcrypto; CREATE TABLE IF NOT EXISTS schema_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());`); err != nil {
		_ = bootTx.Rollback(ctx)
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	for _, name := range names {
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			_ = bootTx.Rollback(ctx)
			return fmt.Errorf("read %s: %w", name, err)
		}
		// Skip if already applied — checked inside the same tx so we
		// don't see stale state from another connection.
		var applied bool
		if err := bootTx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, name).Scan(&applied); err != nil {
			_ = bootTx.Rollback(ctx)
			return fmt.Errorf("check %s: %w", name, err)
		}
		if applied {
			continue
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
