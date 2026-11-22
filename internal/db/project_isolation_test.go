package db

import (
	"context"
	"fmt"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestUniqueProjectSchema pins the 079 invariant: the database, not just
// admin.Create, rejects a second client on the same project schema.
func TestUniqueProjectSchema(t *testing.T) {
	dsn := migrateDSN()
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping unique project schema test")
	}
	requireLocalDB(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM public.clients WHERE tenant_id IN ('key-a','key-b','key-c')`)
	}
	cleanup()
	t.Cleanup(cleanup)

	insert := func(tenant, schema string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO public.clients
			   (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
			 VALUES ($1, $1, $1 || '-hash', 500000, 'active', 'sports', ARRAY['records'], $2)`,
			tenant, schema)
		return err
	}
	if err := insert("key-a", "project_unique_test"); err != nil {
		t.Fatalf("first owner: %v", err)
	}
	if err := insert("key-b", "project_unique_test"); err == nil {
		t.Fatal("a second client on the same project schema was accepted")
	}
	if err := insert("key-c", "project_unique_other"); err != nil {
		t.Fatalf("a distinct project schema should be accepted: %v", err)
	}
}

// TestTwoProjectSchemasAreIsolated is the end-to-end proof of the
// one-project/one-schema promise: two projects migrate into two schemas,
// app_runtime sees only the schema selected for the request, and RLS keeps
// tenant rows apart even when both tables have the same name.
func TestTwoProjectSchemasAreIsolated(t *testing.T) {
	dsn := migrateDSN()
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping two-project isolation test")
	}
	requireLocalDB(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
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
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS project_iso_a CASCADE;
			DROP SCHEMA IF EXISTS project_iso_b CASCADE;
			DELETE FROM public.project_migrations WHERE project_slug IN ('iso_a','iso_b');
			DELETE FROM public.clients WHERE tenant_id IN ('iso-a','iso-b')`)
	}
	cleanup()
	t.Cleanup(cleanup)

	recordsSQL := `
		CREATE TABLE IF NOT EXISTS tenant_records (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			tenant_id text NOT NULL,
			vertical text NOT NULL,
			kind text NOT NULL,
			title text NOT NULL DEFAULT '',
			created_at timestamptz NOT NULL DEFAULT now()
		);
		ALTER TABLE tenant_records ENABLE ROW LEVEL SECURITY;
		ALTER TABLE tenant_records FORCE ROW LEVEL SECURITY;
		DROP POLICY IF EXISTS tenant_records_isolation ON tenant_records;
		CREATE POLICY tenant_records_isolation ON tenant_records
			FOR ALL
			USING (tenant_id = current_setting('app.tenant_id', true))
			WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
	`
	fsys := fstest.MapFS{
		"projects/iso_a/project.json": &fstest.MapFile{Data: []byte(`{
			"slug": "iso_a", "display": "Iso A", "vertical": "sports",
			"schema": "project_iso_a"
		}`)},
		"projects/iso_a/migrations/0001_init.sql": &fstest.MapFile{Data: []byte(recordsSQL)},
		"projects/iso_b/project.json": &fstest.MapFile{Data: []byte(`{
			"slug": "iso_b", "display": "Iso B", "vertical": "sports",
			"schema": "project_iso_b"
		}`)},
		"projects/iso_b/migrations/0001_init.sql": &fstest.MapFile{Data: []byte(recordsSQL)},
	}
	if err := MigrateProjectsFS(ctx, pool, fsys); err != nil {
		t.Fatalf("MigrateProjectsFS: %v", err)
	}

	// app_runtime is the production tenant role: NOBYPASSRLS, not the owner.
	runtimePool, err := NewPool(ctx, toRuntimeDSN(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer runtimePool.Close()

	for i, tc := range []struct {
		tenant string
		schema string
	}{
		{"iso-a", "project_iso_a"},
		{"iso-b", "project_iso_b"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO public.clients
			   (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
			 VALUES ($1, $2, $3, 500000, 'active', 'sports', ARRAY['records'], $4)`,
			tc.tenant, tc.tenant, fmt.Sprintf("iso-hash-%d-%d", time.Now().UnixNano(), i), tc.schema); err != nil {
			t.Fatalf("seed %s: %v", tc.tenant, err)
		}
	}

	withTenant := func(tenant, schema string, fn func(pgx.Tx) error) error {
		tx, err := runtimePool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `
			SELECT set_config('app.tenant_id', $1, true),
			       set_config('search_path', $2, true)`, tenant, schema); err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	count := func(tenant, schema string) int {
		t.Helper()
		var n int
		if err := withTenant(tenant, schema, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM tenant_records`).Scan(&n)
		}); err != nil {
			t.Fatalf("count as %s: %v", tenant, err)
		}
		return n
	}

	if err := withTenant("iso-a", "project_iso_a", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO tenant_records (tenant_id, vertical, kind, title)
			 VALUES ('iso-a', 'sports', 'note', 'A only')`)
		return err
	}); err != nil {
		t.Fatalf("insert A: %v", err)
	}
	if got := count("iso-a", "project_iso_a"); got != 1 {
		t.Fatalf("A sees %d rows, want 1", got)
	}
	if got := count("iso-b", "project_iso_b"); got != 0 {
		t.Fatalf("B sees %d rows from A's schema/tenant, want 0", got)
	}

	if err := withTenant("iso-b", "project_iso_b", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO tenant_records (tenant_id, vertical, kind, title)
			 VALUES ('iso-b', 'sports', 'note', 'B only')`)
		return err
	}); err != nil {
		t.Fatalf("insert B: %v", err)
	}
	if got := count("iso-a", "project_iso_a"); got != 1 {
		t.Fatalf("A sees %d rows after B wrote, want 1", got)
	}
	if got := count("iso-b", "project_iso_b"); got != 1 {
		t.Fatalf("B sees %d rows, want 1", got)
	}

	// The ledger recorded each version against its own schema.
	var aSchema, bSchema string
	if err := pool.QueryRow(ctx,
		`SELECT db_schema FROM public.project_migrations WHERE project_slug='iso_a' AND version='0001_init.sql'`).
		Scan(&aSchema); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT db_schema FROM public.project_migrations WHERE project_slug='iso_b' AND version='0001_init.sql'`).
		Scan(&bSchema); err != nil {
		t.Fatal(err)
	}
	if aSchema != "project_iso_a" || bSchema != "project_iso_b" {
		t.Fatalf("ledger schemas = %q/%q", aSchema, bSchema)
	}
}
