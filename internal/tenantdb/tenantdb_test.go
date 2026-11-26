package tenantdb_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
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

func TestWithTxSchemaUsesResolvedSchemaWithoutLookup(t *testing.T) {
	ctx, pool, schema, tenant := setup(t)
	var gotSchema, gotTenant string
	if err := tenantdb.WithTxSchema(ctx, pool, tenant, schema, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_schema(), current_setting('app.tenant_id')`).
			Scan(&gotSchema, &gotTenant)
	}); err != nil {
		t.Fatal(err)
	}
	if gotSchema != schema || gotTenant != tenant {
		t.Fatalf("schema=%q tenant=%q want %q/%q", gotSchema, gotTenant, schema, tenant)
	}
}

func TestWithTxSchemaRejectsInvalidSchema(t *testing.T) {
	err := tenantdb.WithTxSchema(context.Background(), nil, "t", "project_centrum; drop table x", func(pgx.Tx) error { return nil })
	if err == nil {
		t.Fatal("invalid schema must fail before opening a transaction")
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

// runtimeReadDSN swaps the DSN user for app_runtime without touching the
// host, password, or database, matching the role RLS is enforced for.
func runtimeReadDSN(dsn string) string {
	i := strings.Index(dsn, "://")
	if i < 0 {
		return dsn
	}
	rest := dsn[i+3:]
	at := strings.Index(rest, "@")
	if at < 0 {
		return dsn
	}
	return dsn[:i+3] + "app_runtime" + rest[at:]
}

// TestReadTenantSimpleProtocolRLS proves the one-message fast path keeps
// RLS and search_path semantics: tenant A sees only A's rows, the schema
// resolves, and the transaction-local GUC does not leak back to the pooled
// connection.
func TestReadTenantSimpleProtocolRLS(t *testing.T) {
	ctx, adminPool, schema, tenant := setup(t)
	table := dbtest.Table(schema, "sports_facilities")
	// The facilities FK requires a clients row for every tenant id; create
	// a second active tenant with its own clone (clients.db_schema is
	// unique), then insert a row into THIS clone under that tenant id. RLS
	// must hide it from the first tenant's read.
	otherSchema := dbtest.CloneSchema(t, ctx, adminPool, "project_centrum", "other")
	if _, err := adminPool.Exec(ctx,
		`INSERT INTO public.clients
		   (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
		 VALUES ('other', 'other-tenant', 'hash-other', 1000, 'active', 'sports', ARRAY['records'], $1)`,
		otherSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = adminPool.Exec(cctx, `DELETE FROM public.clients WHERE tenant_id='other-tenant'`)
	})
	if _, err := adminPool.Exec(ctx,
		`INSERT INTO `+table+` (tenant_id, name, type) VALUES ($1,'Court A','badminton'),($2,'Court B','badminton')`,
		tenant, "other-tenant"); err != nil {
		t.Fatal(err)
	}

	dsn := os.Getenv("TEST_DATABASE_URL")
	runtimePool, err := db.NewPoolWithOptions(ctx, runtimeReadDSN(dsn), db.NewPoolOptions{
		MinConns:          0,
		MaxConns:          1, // same connection for the post-read leak check
		UseSimpleProtocol: true,
		ApplicationName:   "tenantdb-read-test",
	})
	if err != nil {
		t.Fatalf("runtime pool: %v", err)
	}
	t.Cleanup(runtimePool.Close)

	var got []string
	err = tenantdb.ReadTenant(ctx, runtimePool, tenant, schema,
		`SELECT current_schema(), current_setting('app.tenant_id'), name
		   FROM sports_facilities ORDER BY name`, nil,
		func(rows pgx.Rows) error {
			for rows.Next() {
				var gotSchema, gotTenant, name string
				if err := rows.Scan(&gotSchema, &gotTenant, &name); err != nil {
					return err
				}
				if gotSchema != schema {
					return fmt.Errorf("current_schema=%q want %q", gotSchema, schema)
				}
				if gotTenant != tenant {
					return fmt.Errorf("app.tenant_id=%q want %q", gotTenant, tenant)
				}
				got = append(got, name)
			}
			return rows.Err()
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "Court A" {
		t.Fatalf("RLS rows=%v, want only [Court A]", got)
	}

	// MaxConns=1 makes this deterministic: the same connection must be
	// back to its default state after the implicit transaction.
	var leaked string
	if err := runtimePool.QueryRow(ctx,
		`SELECT COALESCE(current_setting('app.tenant_id', true), '')`).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != "" {
		t.Fatalf("app.tenant_id leaked after read: %q", leaked)
	}

	// A failed read must not poison the pool: the implicit transaction
	// rolls back server-side, and the next read on the same MaxConns=1
	// pool must work.
	if err := tenantdb.ReadTenant(ctx, runtimePool, tenant, schema,
		`SELECT * FROM does_not_exist`, nil, func(pgx.Rows) error { return nil }); err == nil {
		t.Fatal("broken read must return an error")
	}
	var one int
	if err := tenantdb.ReadTenant(ctx, runtimePool, tenant, schema,
		`SELECT 1`, nil, func(rows pgx.Rows) error {
			if !rows.Next() {
				return pgx.ErrNoRows
			}
			return rows.Scan(&one)
		}); err != nil {
		t.Fatalf("read after failure: %v", err)
	}
	if one != 1 {
		t.Fatalf("read after failure got %d, want 1", one)
	}
}

// TestReadTenantExtendedProtocolFallback covers local/test pools where the
// simple-protocol fast path is unavailable; semantics must be identical.
func TestReadTenantExtendedProtocolFallback(t *testing.T) {
	ctx, pool, schema, tenant := setup(t)
	var gotSchema, gotTenant string
	err := tenantdb.ReadTenant(ctx, pool, tenant, schema,
		`SELECT current_schema(), current_setting('app.tenant_id')`, nil,
		func(rows pgx.Rows) error {
			if !rows.Next() {
				if err := rows.Err(); err != nil {
					return err
				}
				return pgx.ErrNoRows
			}
			return rows.Scan(&gotSchema, &gotTenant)
		})
	if err != nil {
		t.Fatal(err)
	}
	if gotSchema != schema || gotTenant != tenant {
		t.Fatalf("schema=%q tenant=%q want %q/%q", gotSchema, gotTenant, schema, tenant)
	}
}
