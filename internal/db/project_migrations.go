package db

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	projectembed "github.com/davinakmalyasha/agents-proxy"
)

var projectSchemaRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// MigrateAll applies the platform migrations, then every project's own
// migrations. It is the deploy-step entry point (cmd/migrate and boot).
func MigrateAll(ctx context.Context, pool *Pool) error {
	if err := Migrate(ctx, pool); err != nil {
		return err
	}
	return MigrateProjects(ctx, pool)
}

type projectMigrationDoc struct {
	Slug   string `json:"slug"`
	Schema string `json:"schema"`
	// AutoGrant controls the blanket app_runtime DML grant applied after
	// a project's migrations. It defaults to true for new projects. The
	// legacy Centrum project sets it false because its tables already
	// carry deliberate grants (sports_sessions is SELECT-only so the
	// definer functions remain the only write path).
	AutoGrant *bool `json:"auto_grant"`
}

// MigrateProjects applies projects/<slug>/migrations/*.sql inside each
// project's own schema. Project versions are scoped per project, so every
// project may have 0001_init.sql.
//
// The schema is created on first use and granted to app_runtime. Project
// migrations are expected to enable RLS on their tables; the grants here only
// make the tables reachable, not cross-tenant readable (RLS still applies).
func MigrateProjects(ctx context.Context, pool *Pool) error {
	return MigrateProjectsFS(ctx, pool, projectembed.FS)
}

// MigrateProjectsFS is the testable core of MigrateProjects. The fsys must
// contain projects/<dir>/project.json and projects/<dir>/migrations/*.sql.
func MigrateProjectsFS(ctx context.Context, pool *Pool, fsys fs.FS) error {
	entries, err := fs.ReadDir(fsys, "projects")
	if err != nil {
		if strings.Contains(err.Error(), "file does not exist") {
			return nil
		}
		return fmt.Errorf("read projects: %w", err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), "_") {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)

	for _, dir := range dirs {
		doc, err := readProjectDoc(fsys, dir)
		if err != nil {
			return err
		}
		slug := doc.Slug
		if slug == "" {
			slug = strings.ToLower(dir)
		}
		schema := doc.Schema
		if schema == "" {
			schema = "project_" + strings.ReplaceAll(slug, "-", "_")
		}
		if !projectSchemaRe.MatchString(schema) {
			return fmt.Errorf("project %q: invalid schema %q", slug, schema)
		}
		if schema == "public" && slug != "centrum" {
			return fmt.Errorf("project %q: only centrum may use the public schema", slug)
		}

		files, err := projectMigrationFiles(fsys, dir)
		if err != nil {
			return err
		}
		// The legacy Centrum project has no project migrations; its tables
		// are part of the historical platform chain in public. Do not create
		// an empty public-schema step.
		if schema == "public" && len(files) == 0 {
			continue
		}
		autoGrant := doc.AutoGrant == nil || *doc.AutoGrant
		if err := migrateOneProject(ctx, pool, slug, schema, files, autoGrant); err != nil {
			return err
		}
	}
	return nil
}

func readProjectDoc(fsys fs.FS, dir string) (projectMigrationDoc, error) {
	var doc projectMigrationDoc
	raw, err := fs.ReadFile(fsys, path.Join("projects", dir, "project.json"))
	if err != nil {
		if strings.Contains(err.Error(), "file does not exist") {
			return doc, nil
		}
		return doc, fmt.Errorf("project %q: read project.json: %w", dir, err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return doc, fmt.Errorf("project %q: project.json is not valid JSON: %w", dir, err)
	}
	return doc, nil
}

type projectMigrationFile struct {
	Name     string
	Body     []byte
	Checksum string
}

func projectMigrationFiles(fsys fs.FS, dir string) ([]projectMigrationFile, error) {
	root := path.Join("projects", dir, "migrations")
	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		if strings.Contains(err.Error(), "file does not exist") {
			return nil, nil
		}
		return nil, fmt.Errorf("project %q: read migrations: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	out := make([]projectMigrationFile, 0, len(names))
	for _, name := range names {
		body, err := fs.ReadFile(fsys, path.Join(root, name))
		if err != nil {
			return nil, fmt.Errorf("project %q: read %s: %w", dir, name, err)
		}
		out = append(out, projectMigrationFile{
			Name:     name,
			Body:     body,
			Checksum: migrationChecksum(body),
		})
	}
	return out, nil
}

func migrateOneProject(ctx context.Context, pool *Pool, slug, schema string, files []projectMigrationFile, autoGrant bool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("project %q: begin: %w", slug, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, fmt.Sprintf(`SELECT pg_advisory_xact_lock(%d)`, projectMigrationLockID)); err != nil {
		return fmt.Errorf("project %q: lock: %w", slug, err)
	}

	ident := pgx.Identifier{schema}.Sanitize()
	if schema != "public" {
		if _, err := tx.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+ident); err != nil {
			return fmt.Errorf("project %q: create schema %s: %w", slug, schema, err)
		}
		if _, err := tx.Exec(ctx, `GRANT USAGE ON SCHEMA `+ident+` TO app_runtime`); err != nil {
			return fmt.Errorf("project %q: grant schema: %w", slug, err)
		}
	}

	applied, err := readAppliedProjectMigrations(ctx, tx, slug)
	if err != nil {
		return err
	}
	for _, f := range files {
		stored, ok := applied[f.Name]
		if ok {
			if stored == "" {
				if _, err := tx.Exec(ctx,
					`UPDATE public.project_migrations SET checksum = $1
					  WHERE project_slug = $2 AND version = $3 AND checksum = ''`,
					f.Checksum, slug, f.Name); err != nil {
					return fmt.Errorf("project %q: backfill %s: %w", slug, f.Name, err)
				}
				continue
			}
			if stored != f.Checksum {
				return fmt.Errorf("project %q: migration %s was modified after it was applied; add a new migration instead", slug, f.Name)
			}
			continue
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('search_path', $1, true)", schema); err != nil {
			return fmt.Errorf("project %q: set search_path: %w", slug, err)
		}
		if _, err := tx.Exec(ctx, string(f.Body)); err != nil {
			return fmt.Errorf("project %q: apply %s: %w", slug, f.Name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO public.project_migrations (project_slug, version, checksum)
			 VALUES ($1, $2, $3) ON CONFLICT (project_slug, version)
			 DO UPDATE SET checksum = EXCLUDED.checksum`,
			slug, f.Name, f.Checksum); err != nil {
			return fmt.Errorf("project %q: record %s: %w", slug, f.Name, err)
		}
	}

	if schema != "public" {
		// Existing objects and future objects both need the runtime grant.
		// Default privileges are scoped to the migration runner's role.
		// A project with auto_grant=false owns its grants: Centrum's
		// sports_sessions must stay SELECT-only so the definer session
		// functions cannot be bypassed by a direct INSERT/UPDATE/DELETE.
		if autoGrant {
			stmts := []string{
				`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ` + ident + ` TO app_runtime`,
				`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA ` + ident + ` TO app_runtime`,
				`ALTER DEFAULT PRIVILEGES IN SCHEMA ` + ident + ` GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_runtime`,
				`ALTER DEFAULT PRIVILEGES IN SCHEMA ` + ident + ` GRANT USAGE, SELECT ON SEQUENCES TO app_runtime`,
			}
			for _, stmt := range stmts {
				if _, err := tx.Exec(ctx, stmt); err != nil {
					return fmt.Errorf("project %q: grant %s: %w", slug, schema, err)
				}
			}
		}
		// Supabase's default ACL grants anon/authenticated dangerous table
		// privileges (TRUNCATE, REFERENCES, TRIGGER) on newly created tables.
		// Project schemas are never exposed through PostgREST, but revoke them
		// anyway so a later surface cannot inherit the grant.
		for _, role := range []string{"anon", "authenticated"} {
			roleIdent := pgx.Identifier{role}.Sanitize()
			roleLit := "'" + strings.ReplaceAll(role, "'", "''") + "'"
			if _, err := tx.Exec(ctx, fmt.Sprintf(`
				DO $do$
				BEGIN
				  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %[1]s) THEN
				    EXECUTE 'REVOKE ALL ON ALL TABLES IN SCHEMA %[2]s FROM %[3]s';
				    EXECUTE 'REVOKE ALL ON ALL SEQUENCES IN SCHEMA %[2]s FROM %[3]s';
				    EXECUTE 'ALTER DEFAULT PRIVILEGES IN SCHEMA %[2]s REVOKE ALL ON TABLES FROM %[3]s';
				    EXECUTE 'ALTER DEFAULT PRIVILEGES IN SCHEMA %[2]s REVOKE ALL ON SEQUENCES FROM %[3]s';
				  END IF;
				END $do$`, roleLit, ident, roleIdent)); err != nil {
				return fmt.Errorf("project %q: revoke %s on %s: %w", slug, role, schema, err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("project %q: commit: %w", slug, err)
	}
	return nil
}

func readAppliedProjectMigrations(ctx context.Context, tx pgx.Tx, slug string) (map[string]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT version, checksum FROM public.project_migrations WHERE project_slug = $1`, slug)
	if err != nil {
		return nil, fmt.Errorf("project %q: read ledger: %w", slug, err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var version, checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("project %q: read ledger: %w", slug, err)
		}
		out[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("project %q: read ledger: %w", slug, err)
	}
	return out, nil
}
