// Exported helpers so custom features (tenants/<slug>/backend/*)
// can reuse the tenant-transaction and JSON plumbing without living
// in this package.
//
// DONE: PRD 6.5 (custom feature template) / STANDARDS A, D, F
package records

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WithTenantTx opens a transaction, sets app.tenant_id for RLS, runs
// fn, and commits. Custom features use this so every statement is
// tenant-scoped at the SQL level.
func WithTenantTx(ctx context.Context, pool *pgxpool.Pool, tenantID string, fn func(pgx.Tx) error) error {
	return withTenantTx(ctx, pool, tenantID, fn)
}

// WithTenantRead runs one read statement in the tenant's RLS context.
// It is the fast path for single-query GETs: on production's
// simple-protocol pool the SET LOCAL context and the SELECT share one
// round trip. Handlers that need more than one statement must use
// WithTenantTx.
func WithTenantRead(ctx context.Context, pool *pgxpool.Pool, tenantID, sql string, args []any, fn func(pgx.Rows) error) error {
	return withTenantRead(ctx, pool, tenantID, sql, args, fn)
}

// WriteJSON writes a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	writeJSON(w, status, v)
}

// WriteJSONError writes a {"error": msg} JSON response.
func WriteJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSONError(w, status, msg)
}
