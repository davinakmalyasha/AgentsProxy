// Package tenantdb is the tenant-scoped transaction primitive.
//
// Every project handler runs inside a transaction that sets both
// app.tenant_id (for RLS) and search_path (for unqualified project tables).
// Keeping that in one package means the auth provider, the records adapter,
// and every project feature open a tenant transaction the same way.
package tenantdb

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// schemaRe matches every schema the clients table may name. It is
// deliberately wider than project_<slug>: "public" is the platform tenant,
// and a database constraint already rejects anything outside the namespace.
var schemaRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

var (
	// ErrUnknownTenant means clients has no row for the tenant id.
	ErrUnknownTenant = errors.New("tenantdb: unknown tenant")
	// ErrInactiveTenant means the client exists but is not active.
	ErrInactiveTenant = errors.New("tenantdb: inactive tenant")
)

// WithTx opens a transaction, sets the per-statement GUC app.tenant_id, and
// runs fn inside it. RLS policies use the GUC.
//
// The schema is resolved from clients and the transaction fails closed for an
// unknown tenant. There is no public fallback: a caller must name a tenant
// that exists, and a platform tenant must be an explicit db_schema='public'
// row.
//
// A deadlock is retried. It is a normal race, not a malformed request: two
// staff booking the same slot at the same instant both insert, and because
// these tables carry an RLS policy, Postgres cannot satisfy the partial
// EXCLUDE constraint from the index — it re-checks with a heap scan that takes
// row locks, so the two transactions end up waiting on each other and
// PostgreSQL aborts one (SQLSTATE 40P01). Retrying is safe: the aborted
// transaction rolled back, so fn starts from nothing. Only 40P01 is retried.
func WithTx(ctx context.Context, pool *pgxpool.Pool, tenantID string, fn func(pgx.Tx) error) error {
	// Fail closed: an unknown tenant must not silently become a public
	// transaction. Platform tenants are explicit rows with db_schema='public'.
	schema, _, err := Lookup(ctx, pool, tenantID)
	if err != nil {
		return err
	}
	return retryDeadlock(ctx, func() error {
		return withSchemaTxOnce(ctx, pool, tenantID, schema, fn)
	})
}

// WithActiveTx is the fail-closed variant used by authentication. It requires
// a clients row in active status and resolves the schema from that row; an
// unknown or paused tenant returns ErrUnknownTenant / ErrInactiveTenant before
// any project query runs.
func WithActiveTx(ctx context.Context, pool *pgxpool.Pool, tenantID string, fn func(pgx.Tx) error) error {
	schema, status, err := Lookup(ctx, pool, tenantID)
	if err != nil {
		return err
	}
	if status != "active" {
		return ErrInactiveTenant
	}
	return retryDeadlock(ctx, func() error {
		return withSchemaTxOnce(ctx, pool, tenantID, schema, fn)
	})
}

// Lookup resolves the tenant's schema and status. It is read-only and safe to
// call outside a transaction. An unknown tenant is ErrUnknownTenant, not a
// public fallback.
func Lookup(ctx context.Context, pool *pgxpool.Pool, tenantID string) (schema, status string, err error) {
	err = pool.QueryRow(ctx,
		`SELECT db_schema, status FROM public.clients WHERE tenant_id = $1`,
		tenantID).Scan(&schema, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrUnknownTenant
	}
	if err != nil {
		return "", "", fmt.Errorf("tenantdb: lookup tenant: %w", err)
	}
	if !schemaRe.MatchString(schema) {
		return "", "", fmt.Errorf("tenantdb: invalid schema %q for tenant %q", schema, tenantID)
	}
	return schema, status, nil
}

func retryDeadlock(ctx context.Context, run func() error) error {
	const attempts = 3
	for attempt := 1; ; attempt++ {
		err := run()
		var deadlocked *pgconn.PgError
		if attempt >= attempts || !errors.As(err, &deadlocked) || deadlocked.Code != "40P01" {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * 20 * time.Millisecond):
		}
	}
}

func withSchemaTxOnce(ctx context.Context, pool *pgxpool.Pool, tenantID, schema string, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer rollbackOnPanic(ctx, tx)
	if _, err := tx.Exec(ctx,
		`SELECT set_config('app.tenant_id', $1, true), set_config('search_path', $2, true)`,
		tenantID, schema); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("set tenant context: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

func rollbackOnPanic(ctx context.Context, tx pgx.Tx) {
	if p := recover(); p != nil {
		_ = tx.Rollback(ctx)
		panic(p)
	}
}
