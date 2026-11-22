package db

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
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
		var uniqueSchema bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
			  SELECT 1 FROM pg_index i
			    JOIN pg_class c ON c.oid = i.indexrelid
			    JOIN pg_namespace n ON n.oid = c.relnamespace
			   WHERE n.nspname = 'public'
			     AND c.relname = 'clients_project_schema_key'
			     AND i.indisunique)`).Scan(&uniqueSchema); err != nil {
			return nil, fmt.Errorf("inspect clients.db_schema uniqueness: %w", err)
		}
		if !uniqueSchema {
			status.Issues = append(status.Issues,
				"clients.db_schema is not unique; apply migration 079_unique_db_schema.sql")
		}
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
	declaredSchemas := map[string]bool{}
	for _, p := range plans {
		declaredSchemas[p.Schema] = true
	}
	// One project schema, one client. The database enforces this with the
	// partial unique index; the read-only report catches drift on databases
	// that predate 079 or had the index removed by hand.
	schemaOwners := map[string][]string{}
	for tenant, schema := range status.ClientSchemas {
		if strings.HasPrefix(schema, "project_") {
			schemaOwners[schema] = append(schemaOwners[schema], tenant)
		}
	}
	var duplicateSchemas []string
	for schema, tenants := range schemaOwners {
		if len(tenants) > 1 {
			sort.Strings(tenants)
			duplicateSchemas = append(duplicateSchemas, fmt.Sprintf(
				"project schema %q is claimed by multiple clients: %s", schema, strings.Join(tenants, ", ")))
		}
	}
	sort.Strings(duplicateSchemas)
	status.Issues = append(status.Issues, duplicateSchemas...)

	// A client pointed at a project schema that no projects/ folder declares
	// cannot be served reproducibly: the next deploy may not know its
	// migrations or features.
	var undeclared []string
	for tenant, schema := range status.ClientSchemas {
		if strings.HasPrefix(schema, "project_") && !declaredSchemas[schema] {
			undeclared = append(undeclared, fmt.Sprintf(
				"client %q uses project schema %q but no projects/ folder declares it", tenant, schema))
		}
	}
	sort.Strings(undeclared)
	status.Issues = append(status.Issues, undeclared...)

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
		// The ledger must agree with the project's declared schema; otherwise
		// apply would create a new schema and skip every already-recorded
		// version.
		var ledgerMismatch string
		for version, row := range applied {
			if row.Schema != "" && row.Schema != p.Schema {
				ledgerMismatch = fmt.Sprintf(
					"project %q: migration %s was applied to schema %q but project.json uses %q",
					p.Slug, version, row.Schema, p.Schema)
				break
			}
		}
		if ledgerMismatch != "" {
			ps.Issues = append(ps.Issues, ledgerMismatch)
		}
		if schema, ok := status.ClientSchemas[p.Slug]; ok && schema != p.Schema {
			msg := fmt.Sprintf("project %q: clients.db_schema is %q but project.json uses %q", p.Slug, schema, p.Schema)
			ps.Issues = append(ps.Issues, msg)
		}
		rlsIssues, err := auditProjectRLS(ctx, tx, p.Schema)
		if err != nil {
			return nil, err
		}
		shapeIssues, err := auditProjectShape(ctx, tx, p.Schema, len(applied))
		if err != nil {
			return nil, err
		}
		ps.Issues = append(ps.Issues, rlsIssues...)
		ps.Issues = append(ps.Issues, shapeIssues...)
		status.Issues = append(status.Issues, ps.Issues...)
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
	Schema    string
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
	hasSchema, err := hasColumn(ctx, tx, "project_migrations", "db_schema")
	if err != nil {
		return nil, err
	}
	query := `SELECT project_slug, version, checksum, applied_at FROM public.project_migrations`
	if hasSchema {
		query = `SELECT project_slug, version, checksum, db_schema, applied_at FROM public.project_migrations`
	}
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read project_migrations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var slug, version, checksum, schema string
		var applied time.Time
		if hasSchema {
			if err := rows.Scan(&slug, &version, &checksum, &schema, &applied); err != nil {
				return nil, fmt.Errorf("scan project_migrations: %w", err)
			}
		} else if err := rows.Scan(&slug, &version, &checksum, &applied); err != nil {
			return nil, fmt.Errorf("scan project_migrations: %w", err)
		}
		if out[slug] == nil {
			out[slug] = map[string]ledgerRow{}
		}
		out[slug][version] = ledgerRow{Checksum: checksum, Schema: schema, AppliedAt: applied}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read project_migrations: %w", err)
	}
	return out, nil
}

// auditProjectRLS reports tables in a project schema that do not carry the
// ENABLE+FORCE RLS invariant. app_runtime is not the owner, so ENABLE alone
// protects tenant traffic today; FORCE also protects definer/owner paths and
// keeps the schema contract honest.
// auditProjectShape proves the schema the ledger names is actually present.
// A dropped schema that apply recreates empty has no tables; the ledger alone
// would otherwise report clean and provisioning would accept it.
func auditProjectShape(ctx context.Context, tx pgx.Tx, schema string, applied int) ([]string, error) {
	if applied == 0 {
		return nil, nil
	}
	var hasRecords bool
	if err := tx.QueryRow(ctx,
		`SELECT to_regclass(format('%I.tenant_records', $1::text)) IS NOT NULL`, schema).
		Scan(&hasRecords); err != nil {
		return nil, fmt.Errorf("inspect project shape %q: %w", schema, err)
	}
	if !hasRecords {
		return []string{fmt.Sprintf(
			"project schema %q has %d applied migrations but no tenant_records table; restore the schema or reapply the project migrations",
			schema, applied)}, nil
	}
	return nil, nil
}

func auditProjectRLS(ctx context.Context, tx pgx.Tx, schema string) ([]string, error) {
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, schema).Scan(&exists); err != nil {
		return nil, fmt.Errorf("inspect project schema %q: %w", schema, err)
	}
	if !exists {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relkind = 'r'
		 ORDER BY c.relname`, schema)
	if err != nil {
		return nil, fmt.Errorf("inspect project RLS %q: %w", schema, err)
	}
	defer rows.Close()
	var issues []string
	for rows.Next() {
		var table string
		var rls, force bool
		if err := rows.Scan(&table, &rls, &force); err != nil {
			return nil, fmt.Errorf("scan project RLS %q: %w", schema, err)
		}
		if !rls || !force {
			issues = append(issues, fmt.Sprintf(
				"project schema %q: table %q has RLS=%t force=%t; every project table must ENABLE+FORCE RLS",
				schema, table, rls, force))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read project RLS %q: %w", schema, err)
	}
	return issues, nil
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
