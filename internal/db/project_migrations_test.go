package db

import (
	"context"
	"testing"
	"testing/fstest"
	"time"
)

// TestMigrateProjectsFS proves the project-schema path end to end: a project
// gets its own schema, its migration runs with search_path pinned to that
// schema, the ledger records it, and a second run is a no-op.
func TestMigrateProjectsFS(t *testing.T) {
	dsn := migrateDSN()
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping project migration test")
	}
	requireLocalDB(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	pool, err := NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("platform migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS project_dbtest CASCADE;
		DELETE FROM public.project_migrations WHERE project_slug = 'dbtest'`); err != nil {
		t.Fatalf("cleanup before: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS project_dbtest CASCADE;
			DELETE FROM public.project_migrations WHERE project_slug = 'dbtest'`)
	})

	fsys := fstest.MapFS{
		"projects/dbtest/project.json": &fstest.MapFile{Data: []byte(`{
			"slug": "dbtest",
			"display": "DB Test",
			"vertical": "sports",
			"schema": "project_dbtest"
		}`)},
		"projects/dbtest/migrations/0001_init.sql": &fstest.MapFile{Data: []byte(`
			CREATE TABLE IF NOT EXISTS widgets (
				id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
				tenant_id text NOT NULL,
				name text NOT NULL DEFAULT ''
			);
			ALTER TABLE widgets ENABLE ROW LEVEL SECURITY;
			DROP POLICY IF EXISTS widgets_isolation ON widgets;
			CREATE POLICY widgets_isolation ON widgets
				FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
				WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
		`)},
	}

	if err := MigrateProjectsFS(ctx, pool, fsys); err != nil {
		t.Fatalf("MigrateProjectsFS: %v", err)
	}
	var regclass string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('project_dbtest.widgets')::text`).Scan(&regclass); err != nil {
		t.Fatal(err)
	}
	if regclass == "" {
		t.Fatal("project_dbtest.widgets was not created")
	}
	var checksum string
	if err := pool.QueryRow(ctx,
		`SELECT checksum FROM public.project_migrations WHERE project_slug='dbtest' AND version='0001_init.sql'`).
		Scan(&checksum); err != nil {
		t.Fatalf("ledger row: %v", err)
	}
	if checksum == "" {
		t.Fatal("project migration checksum is empty")
	}
	// Second run must not re-apply or fail.
	if err := MigrateProjectsFS(ctx, pool, fsys); err != nil {
		t.Fatalf("MigrateProjectsFS (second): %v", err)
	}
}
