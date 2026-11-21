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
	"time"

	"github.com/jackc/pgx/v5"

	projectembed "github.com/davinakmalyasha/agents-proxy"
)

var projectSchemaRe = regexp.MustCompile(`^project_[a-z0-9_]{1,55}$`)

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
	plans, err := loadProjectPlans(fsys)
	if err != nil {
		return err
	}
	for _, p := range plans {
		if err := migrateOneProject(ctx, pool, p.Slug, p.Schema, p.Files, p.AutoGrant); err != nil {
			return err
		}
	}
	return nil
}

// projectPlan is the filesystem half of a project migration run: identity,
// schema, and every embedded SQL file in apply order. Both the runner and
// the read-only status/check/plan commands use it, so a plan can never
// disagree with what apply would do.
type projectPlan struct {
	Dir       string
	Slug      string
	Schema    string
	AutoGrant bool
	Files     []projectMigrationFile
}

// loadProjectPlans walks projects/ and validates each project's identity and
// schema without touching the database. A missing projects/ directory is an
// empty plan, not an error (unit-test binaries and stripped builds).
func loadProjectPlans(fsys fs.FS) ([]projectPlan, error) {
	entries, err := fs.ReadDir(fsys, "projects")
	if err != nil {
		if strings.Contains(err.Error(), "file does not exist") {
			return nil, nil
		}
		return nil, fmt.Errorf("read projects: %w", err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), "_") {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)

	plans := make([]projectPlan, 0, len(dirs))
	seenSlug := map[string]string{}
	seenSchema := map[string]string{}
	for _, dir := range dirs {
		doc, err := readProjectDoc(fsys, dir)
		if err != nil {
			return nil, err
		}
		slug := doc.Slug
		if slug == "" {
			slug = strings.ToLower(dir)
		}
		if slug != strings.ToLower(dir) {
			return nil, fmt.Errorf("project folder %q declares slug %q; the folder name and slug must match", dir, slug)
		}
		schema := doc.Schema
		if schema == "" {
			schema = "project_" + strings.ReplaceAll(slug, "-", "_")
		}
		if !projectSchemaRe.MatchString(schema) {
			return nil, fmt.Errorf("project %q: invalid schema %q (must match project_<slug>)", slug, schema)
		}
		if other, ok := seenSlug[slug]; ok {
			return nil, fmt.Errorf("project %q: slug is also declared by projects/%s", slug, other)
		}
		seenSlug[slug] = dir
		if other, ok := seenSchema[schema]; ok {
			return nil, fmt.Errorf("project %q: schema %q is also owned by projects/%s", slug, schema, other)
		}
		seenSchema[schema] = dir

		files, err := projectMigrationFiles(fsys, dir)
		if err != nil {
			return nil, err
		}
		plans = append(plans, projectPlan{
			Dir:       dir,
			Slug:      slug,
			Schema:    schema,
			AutoGrant: doc.AutoGrant == nil || *doc.AutoGrant,
			Files:     files,
		})
	}
	return plans, nil
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

	applied, hasSchema, err := readAppliedProjectMigrations(ctx, tx, slug)
	if err != nil {
		return err
	}
	for _, f := range files {
		stored, ok := applied[f.Name]
		if ok {
			if hasSchema && stored.Schema == "" {
				// Legacy row from before migration 075; record the
				// schema now so a later schema edit cannot be mistaken
				// for "already applied".
				if _, err := tx.Exec(ctx,
					`UPDATE public.project_migrations SET db_schema = $1
					  WHERE project_slug = $2 AND version = $3 AND db_schema = ''`,
					schema, slug, f.Name); err != nil {
					return fmt.Errorf("project %q: backfill schema for %s: %w", slug, f.Name, err)
				}
				stored.Schema = schema
			}
			// A project that moved to a different schema must not look
			// migrated in the new schema. The ledger has to record where
			// each version ran; changing that is a deliberate procedure,
			// not a quiet config edit.
			if stored.Schema != "" && stored.Schema != schema {
				return fmt.Errorf(
					"project %q: migration %s was applied to schema %q but project.json now uses %q; changing a project schema requires a deliberate migration procedure",
					slug, f.Name, stored.Schema, schema)
			}
			if stored.Checksum == "" {
				if hasSchema {
					if _, err := tx.Exec(ctx,
						`UPDATE public.project_migrations
						    SET checksum = $1, db_schema = $2
						  WHERE project_slug = $3 AND version = $4 AND checksum = ''`,
						f.Checksum, schema, slug, f.Name); err != nil {
						return fmt.Errorf("project %q: backfill %s: %w", slug, f.Name, err)
					}
				} else if _, err := tx.Exec(ctx,
					`UPDATE public.project_migrations SET checksum = $1
					  WHERE project_slug = $2 AND version = $3 AND checksum = ''`,
					f.Checksum, slug, f.Name); err != nil {
					return fmt.Errorf("project %q: backfill %s: %w", slug, f.Name, err)
				}
				continue
			}
			if stored.Checksum != f.Checksum {
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
		if hasSchema {
			if _, err := tx.Exec(ctx,
				`INSERT INTO public.project_migrations (project_slug, version, checksum, db_schema)
				 VALUES ($1, $2, $3, $4) ON CONFLICT (project_slug, version)
				 DO UPDATE SET checksum = EXCLUDED.checksum, db_schema = EXCLUDED.db_schema`,
				slug, f.Name, f.Checksum, schema); err != nil {
				return fmt.Errorf("project %q: record %s: %w", slug, f.Name, err)
			}
		} else if _, err := tx.Exec(ctx,
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

func readAppliedProjectMigrations(ctx context.Context, tx pgx.Tx, slug string) (map[string]ledgerRow, bool, error) {
	hasSchema, err := hasColumn(ctx, tx, "project_migrations", "db_schema")
	if err != nil {
		return nil, false, fmt.Errorf("project %q: read ledger schema: %w", slug, err)
	}
	query := `SELECT version, checksum, applied_at FROM public.project_migrations WHERE project_slug = $1`
	if hasSchema {
		query = `SELECT version, checksum, db_schema, applied_at FROM public.project_migrations WHERE project_slug = $1`
	}
	rows, err := tx.Query(ctx, query, slug)
	if err != nil {
		return nil, false, fmt.Errorf("project %q: read ledger: %w", slug, err)
	}
	defer rows.Close()
	out := map[string]ledgerRow{}
	for rows.Next() {
		var version, checksum, schema string
		var applied time.Time
		if hasSchema {
			if err := rows.Scan(&version, &checksum, &schema, &applied); err != nil {
				return nil, false, fmt.Errorf("project %q: read ledger: %w", slug, err)
			}
		} else if err := rows.Scan(&version, &checksum, &applied); err != nil {
			return nil, false, fmt.Errorf("project %q: read ledger: %w", slug, err)
		}
		out[version] = ledgerRow{Checksum: checksum, Schema: schema, AppliedAt: applied}
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("project %q: read ledger: %w", slug, err)
	}
	return out, hasSchema, nil
}
