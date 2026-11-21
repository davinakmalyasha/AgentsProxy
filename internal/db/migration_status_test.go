package db

import (
	"context"
	"testing"
	"testing/fstest"
	"time"
)

func TestLoadProjectPlansRejectsDuplicateIdentity(t *testing.T) {
	fsys := fstest.MapFS{
		"projects/a/project.json": &fstest.MapFile{Data: []byte(`{"slug":"same","schema":"project_a"}`)},
		"projects/b/project.json": &fstest.MapFile{Data: []byte(`{"slug":"same","schema":"project_b"}`)},
	}
	if _, err := loadProjectPlans(fsys); err == nil {
		t.Fatal("duplicate slug was accepted")
	}

	fsys = fstest.MapFS{
		"projects/a/project.json": &fstest.MapFile{Data: []byte(`{"slug":"a","schema":"project_same"}`)},
		"projects/b/project.json": &fstest.MapFile{Data: []byte(`{"slug":"b","schema":"project_same"}`)},
	}
	if _, err := loadProjectPlans(fsys); err == nil {
		t.Fatal("duplicate schema was accepted")
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
	defer pool.Close()
	if err := MigrateAll(ctx, pool); err != nil {
		t.Fatalf("MigrateAll: %v", err)
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
