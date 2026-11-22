package tenantdb_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/internal/dbtest"
	"github.com/davinakmalyasha/agents-proxy/internal/tenantdb"
	"github.com/davinakmalyasha/agents-proxy/internal/testsafety"
)

func setup(t *testing.T) (context.Context, *pgxpool.Pool, string, string) {
	t.Helper()
	testsafety.RequireLocal(t)
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping tenantdb test")
	}
	adminDSN := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if adminDSN == "" {
		adminDSN = dsn
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	pool, err := db.NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.MigrateAll(ctx, pool); err != nil {
		t.Fatal(err)
	}

	const tenant = "tdb-test"
	cleanup := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		_, _ = pool.Exec(cctx, `DELETE FROM public.clients WHERE tenant_id=$1`, tenant)
	}
	cleanup()
	t.Cleanup(cleanup)
	schema := dbtest.CloneSchema(t, ctx, pool, "project_centrum", tenant)
	if _, err := pool.Exec(ctx,
		`INSERT INTO public.clients
		   (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
		 VALUES ('tenantdb', $1, 'hash-tdb', 1000, 'active', 'sports', ARRAY['records'], $2)`,
		tenant, schema); err != nil {
		t.Fatal(err)
	}
	return ctx, pool, schema, tenant
}

func TestWithTxSetsTenantContextAndSearchPath(t *testing.T) {
	ctx, pool, schema, tenant := setup(t)
	var gotSchema, gotTenant string
	if err := tenantdb.WithTx(ctx, pool, tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT current_schema(), current_setting('app.tenant_id')`).
			Scan(&gotSchema, &gotTenant); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO sports_facilities (tenant_id, name, type) VALUES ($1,'Court TDB','badminton')`, tenant)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if gotSchema != schema || gotTenant != tenant {
		t.Fatalf("schema=%q tenant=%q want %q/%q", gotSchema, gotTenant, schema, tenant)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM `+dbtest.Table(schema, "sports_facilities")+` WHERE name='Court TDB'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("clone rows=%d want 1", n)
	}
}

func TestLookupAndActiveTxFailClosed(t *testing.T) {
	ctx, pool, schema, tenant := setup(t)
	gotSchema, status, err := tenantdb.Lookup(ctx, pool, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if gotSchema != schema || status != "active" {
		t.Fatalf("lookup=%q/%q want %q/active", gotSchema, status, schema)
	}
	if err := tenantdb.WithActiveTx(ctx, pool, tenant, func(pgx.Tx) error { return nil }); err != nil {
		t.Fatalf("active tenant: %v", err)
	}
	if _, _, err := tenantdb.Lookup(ctx, pool, "missing-tenant"); !errors.Is(err, tenantdb.ErrUnknownTenant) {
		t.Fatalf("unknown tenant err=%v", err)
	}
	if err := tenantdb.WithActiveTx(ctx, pool, "missing-tenant", func(pgx.Tx) error { return nil }); !errors.Is(err, tenantdb.ErrUnknownTenant) {
		t.Fatalf("unknown tenant tx err=%v", err)
	}
	if err := tenantdb.WithTx(ctx, pool, "missing-tenant", func(pgx.Tx) error { return nil }); !errors.Is(err, tenantdb.ErrUnknownTenant) {
		t.Fatalf("WithTx unknown tenant err=%v, want ErrUnknownTenant (no public fallback)", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE public.clients SET status='paused' WHERE tenant_id=$1`, tenant); err != nil {
		t.Fatal(err)
	}
	if err := tenantdb.WithActiveTx(ctx, pool, tenant, func(pgx.Tx) error { return nil }); !errors.Is(err, tenantdb.ErrInactiveTenant) {
		t.Fatalf("paused tenant tx err=%v", err)
	}
}
