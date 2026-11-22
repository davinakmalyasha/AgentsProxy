package db

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestLoadProjectPlansRejectsManifestIdentity(t *testing.T) {
	// Folder and slug are the same identity; a folder cannot claim another
	// project's slug.
	fsys := fstest.MapFS{
		"projects/a/project.json": &fstest.MapFile{Data: []byte(`{"slug":"same","schema":"project_same"}`)},
	}
	if _, err := loadProjectPlans(fsys); err == nil || !strings.Contains(err.Error(), "folder name and slug must match") {
		t.Fatalf("folder/slug mismatch error = %v", err)
	}

	// Exact schema derivation makes a shared project_* schema impossible
	// through the manifest; the rejection must still be explicit.
	fsys = fstest.MapFS{
		"projects/a/project.json": &fstest.MapFile{Data: []byte(`{"slug":"a","schema":"project_same"}`)},
	}
	if _, err := loadProjectPlans(fsys); err == nil || !strings.Contains(err.Error(), "must be") {
		t.Fatalf("non-derived schema error = %v", err)
	}
}

func TestLoadProjectPlansDefaultsAndSkipsPrivateDirs(t *testing.T) {
	fsys := fstest.MapFS{
		"projects/_template/project.json": &fstest.MapFile{Data: []byte(`{"slug":"template"}`)},
		"projects/acme/project.json":      &fstest.MapFile{Data: []byte(`{"vertical":"sports"}`)},
		"projects/acme/migrations/0001_init.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE t (id int);\r\n"),
		},
	}
	plans, err := loadProjectPlans(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatalf("got %d plans, want 1", len(plans))
	}
	p := plans[0]
	if p.Slug != "acme" {
		t.Errorf("slug=%q, want acme", p.Slug)
	}
	if p.Schema != "project_acme" {
		t.Errorf("schema=%q, want project_acme", p.Schema)
	}
	if !p.AutoGrant {
		t.Error("auto_grant should default true")
	}
	if len(p.Files) != 1 || p.Files[0].Checksum == "" {
		t.Fatalf("migration files not loaded: %+v", p.Files)
	}
}

func TestCompareFilesSeparatesPendingDriftAndMissing(t *testing.T) {
	files := []projectMigrationFile{
		{Name: "001_a.sql", Checksum: "aaa", Body: []byte("a")},
		{Name: "002_b.sql", Checksum: "bbb", Body: []byte("b")},
	}
	applied := map[string]ledgerRow{
		"001_a.sql": {Checksum: "aaa", AppliedAt: time.Now()},
		"002_b.sql": {Checksum: "old", AppliedAt: time.Now()},
		"003_c.sql": {Checksum: "ccc", AppliedAt: time.Now()},
	}
	var status MigrationStatus
	entries := compareFiles(files, applied, true, &status)
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	if entries[0].State != MigrationApplied {
		t.Errorf("001 state=%s, want applied", entries[0].State)
	}
	if entries[1].State != MigrationDrift {
		t.Errorf("002 state=%s, want drift", entries[1].State)
	}
	if entries[2].State != MigrationMissing {
		t.Errorf("003 state=%s, want missing", entries[2].State)
	}
	if status.Pending != 0 {
		t.Errorf("pending=%d, want 0", status.Pending)
	}
	if len(status.Issues) != 2 {
		t.Errorf("issues=%v, want 2", status.Issues)
	}
}

func TestCompareProjectFilesCountsPending(t *testing.T) {
	files := []projectMigrationFile{{Name: "0001_init.sql", Checksum: "aaa"}}
	var ps ProjectMigrationStatus
	entries := compareProjectFiles(files, map[string]ledgerRow{}, &ps)
	if len(entries) != 1 || entries[0].State != MigrationPending {
		t.Fatalf("entries=%+v", entries)
	}
	if len(ps.Issues) != 0 {
		t.Errorf("pending is not an issue: %v", ps.Issues)
	}
}

func TestInspectMigrationsDetectsRLSGap(t *testing.T) {
	dsn := migrateDSN()
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping RLS audit test")
	}
	requireLocalDB(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	pool, err := NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("platform migrate: %v", err)
	}
	cleanup := func() {
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS project_rlsgap CASCADE;
			DELETE FROM public.project_migrations WHERE project_slug='rlsgap';
			DELETE FROM public.clients WHERE tenant_id='rls-gap'`)
	}
	cleanup()
	t.Cleanup(cleanup)

	fsys := fstest.MapFS{
		"projects/rlsgap/project.json": &fstest.MapFile{Data: []byte(`{
			"slug": "rlsgap", "vertical": "sports", "schema": "project_rlsgap"
		}`)},
		"projects/rlsgap/migrations/0001_init.sql": &fstest.MapFile{Data: []byte(`
			CREATE TABLE widgets (
				id int PRIMARY KEY,
				tenant_id text NOT NULL
			);
			ALTER TABLE widgets ENABLE ROW LEVEL SECURITY;
			CREATE POLICY widgets_isolation ON widgets
				USING (tenant_id = current_setting('app.tenant_id', true));
		`)},
	}
	if err := MigrateProjectsFS(ctx, pool, fsys); err != nil {
		t.Fatalf("project migrate: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO public.clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
		 VALUES ('RLS Gap','rls-gap','rls-gap-hash',500000,'active','sports',ARRAY['records'],'project_rlsgap')`); err != nil {
		t.Fatal(err)
	}
	status, err := InspectMigrationsFS(ctx, pool, fsys)
	if err != nil {
		t.Fatalf("InspectMigrationsFS: %v", err)
	}
	found := false
	for _, issue := range status.Issues {
		if strings.Contains(issue, "ENABLE+FORCE RLS") && strings.Contains(issue, "project_rlsgap") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("RLS gap not reported: %v", status.Issues)
	}
}

func TestInspectMigrationsCleanAfterApply(t *testing.T) {
	dsn := migrateDSN()
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping status integration test")
	}
	requireLocalDB(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	pool, err := NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := MigrateAll(ctx, pool); err != nil {
		t.Fatalf("MigrateAll: %v", err)
	}
	// This test asserts a globally clean report, so remove leftover demo
	// tenants/ledgers from other integration packages. It is local-only
	// (requireLocalDB) and runs with `-p 1` in CI.
	if _, err := pool.Exec(ctx,
		`DELETE FROM public.clients WHERE tenant_id NOT IN ('centrum','dfd-agency');
		 DELETE FROM public.project_migrations WHERE project_slug <> 'centrum'`); err != nil {
		t.Fatalf("cleanup test tenants: %v", err)
	}
	status, err := InspectMigrations(ctx, pool)
	if err != nil {
		t.Fatalf("InspectMigrations: %v", err)
	}
	if !status.Clean() {
		t.Fatalf("status is not clean: pending=%d issues=%v", status.Pending, status.Issues)
	}
	if !status.PlatformInitialized || !status.ProjectInitialized {
		t.Fatalf("ledgers not initialized: platform=%v project=%v",
			status.PlatformInitialized, status.ProjectInitialized)
	}
	if len(status.Platform) == 0 || len(status.Projects) == 0 {
		t.Fatalf("empty report: platform=%d projects=%d", len(status.Platform), len(status.Projects))
	}
}
