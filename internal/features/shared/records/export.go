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

	"github.com/davinakmalyasha/agents-proxy/internal/cache"
)

// WithTenantTx opens a transaction, sets app.tenant_id for RLS, runs
// fn, and commits. Custom features use this so every statement is
// tenant-scoped at the SQL level.
func WithTenantTx(ctx context.Context, pool *pgxpool.Pool, tenantID string, fn func(pgx.Tx) error) error {
	return withTenantTx(ctx, pool, tenantID, fn)
}

// RequireClient reads the authenticated client from the request
// context. It writes 401 and returns nil if the auth middleware was
// bypassed.
func RequireClient(w http.ResponseWriter, r *http.Request) *cache.ClientModel {
	return requireClient(w, r)
}

// WriteJSON writes a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	writeJSON(w, status, v)
}

// WriteJSONError writes a {"error": msg} JSON response.
func WriteJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSONError(w, status, msg)
}
