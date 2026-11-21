package db

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	projectembed "github.com/davinakmalyasha/agents-proxy"
)

// Migration state is the read-only view shared by `cmd/migrate status`,
// `check`, and `plan`. It deliberately reuses projectPlan, the same
// filesystem loader the apply path uses, so a report can never describe a
// different set of files than MigrateAll would execute.
type MigrationState string

const (
	MigrationApplied MigrationState = "applied"
	MigrationPending MigrationState = "pending"
	MigrationDrift   MigrationState = "drift"
	MigrationMissing MigrationState = "missing"
)

// MigrationEntry is one migration file (or ledger row with no file).
type MigrationEntry struct {
	Version         string         `json:"version"`
	State           MigrationState `json:"state"`
	FileChecksum    string         `json:"file_checksum,omitempty"`
	AppliedChecksum string         `json:"applied_checksum,omitempty"`
	AppliedAt       time.Time      `json:"applied_at,omitempty"`
	SizeBytes       int            `json:"size_bytes,omitempty"`
}

// ProjectMigrationStatus is one project's schema and ledger state.
type ProjectMigrationStatus struct {
	Slug       string           `json:"slug"`
	Schema     string           `json:"schema"`
	AutoGrant  bool             `json:"auto_grant"`
	Migrations []MigrationEntry `json:"migrations"`
	Issues     []string         `json:"issues,omitempty"`
}

// MigrationStatus is the complete read-only report.
type MigrationStatus struct {
	PlatformInitialized bool                     `json:"platform_initialized"`
	ProjectInitialized  bool                     `json:"project_initialized"`
	Platform            []MigrationEntry         `json:"platform"`
	Projects            []ProjectMigrationStatus `json:"projects"`
	ClientSchemas       map[string]string        `json:"client_schemas,omitempty"`
	Issues              []string                 `json:"issues,omitempty"`
	Pending             int                      `json:"pending"`
}

// Clean reports whether the database is fully migrated and every applied
// migration still matches the build. It is what `migrate check` exits on.
func (s *MigrationStatus) Clean() bool {
	return len(s.Issues) == 0 && s.Pending == 0
}

// InspectMigrations reads both ledgers and every embedded migration file
// without applying anything. It runs inside a READ ONLY transaction: a
// status command pointed at production must be incapable of writing, even if
// a future edit accidentally adds an Exec.
func InspectMigrations(ctx context.Context, pool *Pool) (*MigrationStatus, error) {
	return InspectMigrationsFS(ctx, pool, projectembed.FS)
}

// InspectMigrationsFS is the testable core of InspectMigrations.
func InspectMigrationsFS(ctx context.Context, pool *Pool, fsys fs.FS) (*MigrationStatus, error) {
	plans, err := loadProjectPlans(fsys)
	if err != nil {
		return nil, err
	}
	platformFiles, err := platformMigrationFiles()
	if err != nil {
		return nil, err
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin read-only status: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var hasPlatform, hasProject, hasClients bool
	if err := tx.QueryRow(ctx, `SELECT
		to_regclass('public.schema_migrations') IS NOT NULL,
		to_regclass('public.project_migrations') IS NOT NULL,
		to_regclass('public.clients') IS NOT NULL`).Scan(&hasPlatform, &hasProject, &hasClients); err != nil {
		return nil, fmt.Errorf("inspect ledgers: %w", err)
	}

	status := &MigrationStatus{
		PlatformInitialized: hasPlatform,
		ProjectInitialized:  hasProject,
		ClientSchemas:       map[string]string{},
	}

	platformApplied, err := readPlatformLedger(ctx, tx, hasPlatform)
	if err != nil {
		return nil, err
	}
	status.Platform = compareFiles(platformFiles, platformApplied, true, status)
	if !hasPlatform && len(status.Platform) > 0 {
		status.Issues = append(status.Issues,
			"platform ledger does not exist; the next apply will create it")
	}

	projectApplied, err := readProjectLedger(ctx, tx, hasProject)
	if err != nil {
		return nil, err
	}
	if !hasProject && len(plans) > 0 {
		status.Issues = append(status.Issues,
			"project ledger does not exist; the next apply will create it")
	}

	if hasClients {
		rows, err := tx.Query(ctx, `SELECT tenant_id, db_schema FROM public.clients`)
		if err != nil {
			return nil, fmt.Errorf("read client schemas: %w", err)
		}
		for rows.Next() {
			var tenant, schema string
			if err := rows.Scan(&tenant, &schema); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan client schema: %w", err)
			}
			status.ClientSchemas[tenant] = schema
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read client schemas: %w", err)
		}
		rows.Close()
	}

	seenProjects := map[string]bool{}
	for _, p := range plans {
		ps := ProjectMigrationStatus{
			Slug:      p.Slug,
			Schema:    p.Schema,
			AutoGrant: p.AutoGrant,
		}
		applied := projectApplied[p.Slug]
		ps.Migrations = compareProjectFiles(p.Files, applied, &ps)
		seenProjects[p.Slug] = true
		for _, entry := range ps.Migrations {
			if entry.State == MigrationPending {
				status.Pending++
			}
		}
		status.Issues = append(status.Issues, ps.Issues...)
		if schema, ok := status.ClientSchemas[p.Slug]; ok && schema != p.Schema {
			msg := fmt.Sprintf("project %q: clients.db_schema is %q but project.json uses %q", p.Slug, schema, p.Schema)
			ps.Issues = append(ps.Issues, msg)
			status.Issues = append(status.Issues, msg)
		}
		status.Projects = append(status.Projects, ps)
	}

	// Ledger rows for a project whose folder disappeared from this build are
	// a deployment error: the code no longer knows how to reproduce that
	// schema, but the database still claims it exists.
	var orphanSlugs []string
	for slug := range projectApplied {
		if !seenProjects[slug] {
			orphanSlugs = append(orphanSlugs, slug)
		}
	}
	sort.Strings(orphanSlugs)
	for _, slug := range orphanSlugs {
		ps := ProjectMigrationStatus{Slug: slug}
		for version, row := range projectApplied[slug] {
			ps.Migrations = append(ps.Migrations, MigrationEntry{
				Version:         version,
				State:           MigrationMissing,
				AppliedChecksum: row.Checksum,
				AppliedAt:       row.AppliedAt,
			})
		}
		sort.Slice(ps.Migrations, func(i, j int) bool {
			return ps.Migrations[i].Version < ps.Migrations[j].Version
		})
		msg := fmt.Sprintf("project %q is recorded in project_migrations but has no projects/ folder in this build", slug)
		ps.Issues = append(ps.Issues, msg)
		status.Issues = append(status.Issues, msg)
		status.Projects = append(status.Projects, ps)
	}

	return status, nil
}

// platformMigrationFiles returns the embedded platform chain in apply order.
func platformMigrationFiles() ([]projectMigrationFile, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && hasSQLSuffix(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	out := make([]projectMigrationFile, 0, len(names))
	for _, name := range names {
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		out = append(out, projectMigrationFile{
			Name:     name,
			Body:     body,
			Checksum: migrationChecksum(body),
		})
	}
	return out, nil
}

type ledgerRow struct {
	Checksum  string
	AppliedAt time.Time
}

func readPlatformLedger(ctx context.Context, tx pgx.Tx, exists bool) (map[string]ledgerRow, error) {
	out := map[string]ledgerRow{}
	if !exists {
		return out, nil
	}
	hasChecksum, err := hasColumn(ctx, tx, "schema_migrations", "checksum")
	if err != nil {
		return nil, err
	}
	query := `SELECT version, applied_at FROM public.schema_migrations`
	if hasChecksum {
		query = `SELECT version, checksum, applied_at FROM public.schema_migrations`
	}
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var version string
		var applied time.Time
		var checksum string
		if hasChecksum {
			if err := rows.Scan(&version, &checksum, &applied); err != nil {
				return nil, fmt.Errorf("scan schema_migrations: %w", err)
			}
		} else if err := rows.Scan(&version, &applied); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		out[version] = ledgerRow{Checksum: checksum, AppliedAt: applied}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	return out, nil
}

func readProjectLedger(ctx context.Context, tx pgx.Tx, exists bool) (map[string]map[string]ledgerRow, error) {
	out := map[string]map[string]ledgerRow{}
	if !exists {
		return out, nil
	}
	rows, err := tx.Query(ctx,
		`SELECT project_slug, version, checksum, applied_at FROM public.project_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read project_migrations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var slug, version, checksum string
		var applied time.Time
		if err := rows.Scan(&slug, &version, &checksum, &applied); err != nil {
			return nil, fmt.Errorf("scan project_migrations: %w", err)
		}
		if out[slug] == nil {
			out[slug] = map[string]ledgerRow{}
		}
		out[slug][version] = ledgerRow{Checksum: checksum, AppliedAt: applied}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read project_migrations: %w", err)
	}
	return out, nil
}

func hasColumn(ctx context.Context, tx pgx.Tx, table, column string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2)`,
		table, column).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("inspect %s.%s: %w", table, column, err)
	}
	return ok, nil
}

func compareFiles(files []projectMigrationFile, applied map[string]ledgerRow, platform bool, status *MigrationStatus) []MigrationEntry {
	entries := make([]MigrationEntry, 0, len(files)+len(applied))
	seen := map[string]bool{}
	for _, f := range files {
		entry := MigrationEntry{
			Version:      f.Name,
			FileChecksum: f.Checksum,
			SizeBytes:    len(f.Body),
			State:        MigrationPending,
		}
		if row, ok := applied[f.Name]; ok {
			entry.AppliedChecksum = row.Checksum
			entry.AppliedAt = row.AppliedAt
			switch {
			case row.Checksum != "" && row.Checksum != f.Checksum:
				entry.State = MigrationDrift
				status.Issues = append(status.Issues,
					fmt.Sprintf("%s migration %s was modified after it was applied", ledgerLabel(platform), f.Name))
			default:
				entry.State = MigrationApplied
			}
		}
		if entry.State == MigrationPending {
			status.Pending++
		}
		entries = append(entries, entry)
		seen[f.Name] = true
	}
	var orphans []string
	for version := range applied {
		if !seen[version] {
			orphans = append(orphans, version)
		}
	}
	sort.Strings(orphans)
	for _, version := range orphans {
		row := applied[version]
		entries = append(entries, MigrationEntry{
			Version:         version,
			State:           MigrationMissing,
			AppliedChecksum: row.Checksum,
			AppliedAt:       row.AppliedAt,
		})
		status.Issues = append(status.Issues,
			fmt.Sprintf("%s migration %s is in the ledger but missing from this build", ledgerLabel(platform), version))
	}
	return entries
}

func compareProjectFiles(files []projectMigrationFile, applied map[string]ledgerRow, ps *ProjectMigrationStatus) []MigrationEntry {
	entries := make([]MigrationEntry, 0, len(files)+len(applied))
	seen := map[string]bool{}
	for _, f := range files {
		entry := MigrationEntry{
			Version:      f.Name,
			FileChecksum: f.Checksum,
			SizeBytes:    len(f.Body),
			State:        MigrationPending,
		}
		if row, ok := applied[f.Name]; ok {
			entry.AppliedChecksum = row.Checksum
			entry.AppliedAt = row.AppliedAt
			if row.Checksum != "" && row.Checksum != f.Checksum {
				entry.State = MigrationDrift
				msg := fmt.Sprintf("project %q: migration %s was modified after it was applied", ps.Slug, f.Name)
				ps.Issues = append(ps.Issues, msg)
			} else {
				entry.State = MigrationApplied
			}
		}
		entries = append(entries, entry)
		seen[f.Name] = true
	}
	var orphans []string
	for version := range applied {
		if !seen[version] {
			orphans = append(orphans, version)
		}
	}
	sort.Strings(orphans)
	for _, version := range orphans {
		row := applied[version]
		entries = append(entries, MigrationEntry{
			Version:         version,
			State:           MigrationMissing,
			AppliedChecksum: row.Checksum,
			AppliedAt:       row.AppliedAt,
		})
		msg := fmt.Sprintf("project %q: migration %s is in the ledger but missing from this build", ps.Slug, version)
		ps.Issues = append(ps.Issues, msg)
	}
	return entries
}

func ledgerLabel(platform bool) string {
	if platform {
		return "platform"
	}
	return "project"
}

func hasSQLSuffix(name string) bool {
	return len(name) > 4 && name[len(name)-4:] == ".sql"
}
