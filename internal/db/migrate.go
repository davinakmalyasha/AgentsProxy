// DONE: PRD 4 (idempotent migrations, advisory lock) / STANDARDS A, H
package db

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationLockID serialises concurrent platform bootstraps. A collision
// with any other advisory lock in the system is an invisible deadlock, so
// it lives here as a named constant, not a literal.
const migrationLockID = 734561

// projectMigrationLockID serialises project migration runners. It is
// deliberately different from the platform lock: platform migrations and
// project migrations are separate steps and must not block each other into
// an invisible deadlock.
const projectMigrationLockID = 734562

// allMigrationLockID serialises a whole MigrateAll (platform then project).
// It is held on a dedicated session connection; the inner transaction locks
// still protect direct Migrate/MigrateProjects callers.
const allMigrationLockID = 734560

// Migrate applies every embedded platform migration in lexical order,
// recording each in schema_migrations. All migrations are idempotent
// (IF NOT EXISTS / ON CONFLICT DO NOTHING), so re-running is safe.
//
// The ledger now stores a SHA-256 checksum. An applied migration whose file
// changed after it was applied fails the boot instead of silently drifting
// from the schema it produced. Legacy rows with an empty checksum are
// backfilled on the first run (trust-on-first-use), so the rollout does not
// require a manual data migration.
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

	// Read once: checksum verification needs every file, not just pending.
	bodies := make(map[string][]byte, len(names))
	checksums := make(map[string]string, len(names))
	for _, name := range names {
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		bodies[name] = body
		checksums[name] = migrationChecksum(body)
	}

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
	if _, err := bootTx.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pgcrypto;
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now(),
			checksum text NOT NULL DEFAULT ''
		);
		ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS checksum text NOT NULL DEFAULT '';`); err != nil {
		_ = bootTx.Rollback(ctx)
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}
	applied, err := readApplied(ctx, bootTx)
	if err != nil {
		_ = bootTx.Rollback(ctx)
		return err
	}
	pending, err := verifyAndBackfill(ctx, bootTx, names, checksums, applied)
	if err != nil {
		_ = bootTx.Rollback(ctx)
		return err
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
		applied, err = readApplied(ctx, bootTx)
		if err != nil {
			_ = bootTx.Rollback(ctx)
			return err
		}
		pending, err = verifyAndBackfill(ctx, bootTx, names, checksums, applied)
		if err != nil {
			_ = bootTx.Rollback(ctx)
			return err
		}
		if len(pending) == 0 {
			if err := bootTx.Commit(ctx); err != nil {
				return fmt.Errorf("commit migrations: %w", err)
			}
			return nil
		}
	}

	for _, name := range pending {
		if _, err := bootTx.Exec(ctx, string(bodies[name])); err != nil {
			_ = bootTx.Rollback(ctx)
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := bootTx.Exec(ctx,
			`INSERT INTO schema_migrations(version, checksum) VALUES ($1, $2)
			 ON CONFLICT (version) DO UPDATE SET checksum = EXCLUDED.checksum`,
			name, checksums[name]); err != nil {
			_ = bootTx.Rollback(ctx)
			return fmt.Errorf("record %s: %w", name, err)
		}
	}
	if err := bootTx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	return nil
}

// migrationChecksum hashes SQL content with line endings normalized. A
// Windows checkout (CRLF) and a Linux deployment (LF) must produce the same
// checksum for the same migration.
func migrationChecksum(body []byte) string {
	normalized := strings.ReplaceAll(string(body), "\r\n", "\n")
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

func shortHash(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// readApplied returns version -> checksum for the platform ledger.
func readApplied(ctx context.Context, tx pgx.Tx) (map[string]string, error) {
	rows, err := tx.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	applied := map[string]string{}
	for rows.Next() {
		var version, checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("read schema_migrations: %w", err)
		}
		applied[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	return applied, nil
}

// verifyAndBackfill checks every applied migration against its file checksum
// and returns the pending files. A missing checksum is backfilled (legacy
// ledger); a mismatch is a hard error.
func verifyAndBackfill(ctx context.Context, tx pgx.Tx, names []string, checksums map[string]string, applied map[string]string) ([]string, error) {
	for version := range applied {
		if _, ok := checksums[version]; !ok {
			return nil, fmt.Errorf("database has applied migration %s but the file is missing from this build; restore the file or add a repair migration", version)
		}
	}
	var pending []string
	for _, name := range names {
		stored, ok := applied[name]
		if !ok {
			pending = append(pending, name)
			continue
		}
		if stored == "" {
			if _, err := tx.Exec(ctx,
				`UPDATE schema_migrations SET checksum = $1 WHERE version = $2 AND checksum = ''`,
				checksums[name], name); err != nil {
				return nil, fmt.Errorf("backfill checksum %s: %w", name, err)
			}
			continue
		}
		if stored != checksums[name] {
			return nil, fmt.Errorf("migration %s was modified after it was applied (ledger %s, file %s); add a new migration instead of editing an applied one", name, shortHash(stored), shortHash(checksums[name]))
		}
	}
	return pending, nil
}
