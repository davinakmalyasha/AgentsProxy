// Package records is the shared "records" feature — the generic CRUD
// engine over tenant_records. It also provides the per-request auth
// helper that every other shared feature uses.
//
// DONE: PRD 6.5 (records feature) / STANDARDS A, D, F
package records

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/features/clientctx"
	"github.com/davinakmalyasha/agents-proxy/internal/middleware"
	"github.com/davinakmalyasha/agents-proxy/internal/tenantdb"
)

// Deps is what handlers and the routes builder need.
type Deps struct {
	Pool  *pgxpool.Pool
	Cache *cache.Client
}

// WithClientAuth is the exported version of the per-request auth
// middleware. It is the single place every business endpoint
// resolves X-Client-Token and stashes the *cache.ClientModel.
func WithClientAuth(deps Deps) func(http.Handler) http.Handler { return withClientAuth(deps) }

// withClientAuth reads X-Client-Token, validates via cache, and
// stashes the resolved client (and tenant) into the request context.
// 401 on bad/missing, 403 on paused/blocked, 500 on internal.
func withClientAuth(deps Deps) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := r.Header.Get("X-Client-Token")
			if tok == "" {
				writeJSONError(w, http.StatusUnauthorized, "missing X-Client-Token")
				return
			}
			cm, err := deps.Cache.GetByToken(r.Context(), tok)
			if err != nil {
				if errors.Is(err, cache.ErrInvalidToken) {
					writeJSONError(w, http.StatusUnauthorized, "invalid token")
					return
				}
				slog.Error("client auth lookup", "err", err)
				writeJSONError(w, http.StatusInternalServerError, "internal")
				return
			}
			if cm.Status != "active" {
				writeJSONError(w, http.StatusForbidden, "client "+cm.Status)
				return
			}
			ctx := clientctx.WithClient(r.Context(), cm)
			ctx = middleware.SetClientID(ctx, cm.ID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// withTenantTx opens a transaction, sets the per-statement GUC
// `app.tenant_id`, and runs fn inside it. RLS policies use the GUC.
// The connection is held for the duration of fn — never call this
// and then return a `pgx.Tx` to a goroutine.
// withTenantTx runs fn in a transaction with the tenant RLS context set.
// The implementation lives in internal/tenantdb so the auth provider and
// project features use the same primitive without importing the records
// feature package.
//
// The resolved identity already carries the tenant's db_schema. Passing it
// through skips a clients lookup per request; callers without an identity
// (agent tools, background paths) fall back to the cached lookup in WithTx.
func withTenantTx(ctx context.Context, pool *pgxpool.Pool, tenantID string, fn func(pgx.Tx) error) error {
	if id := clientctx.From(ctx); id != nil && id.TenantID() == tenantID && id.DBSchema != "" {
		return tenantdb.WithTxSchema(ctx, pool, tenantID, id.DBSchema, fn)
	}
	return tenantdb.WithTx(ctx, pool, tenantID, fn)
}

// withTenantRead runs one read statement in the tenant's RLS context.
// Unlike withTenantTx it does not open an explicit transaction: on a
// simple-protocol pool (production) the context and the read share one
// simple Query message, which PostgreSQL executes as one implicit
// transaction. That removes three round trips from every single-query
// public read. Multi-statement handlers must keep using withTenantTx.
func withTenantRead(ctx context.Context, pool *pgxpool.Pool, tenantID, sql string, args []any, fn func(pgx.Rows) error) error {
	schema := ""
	if id := clientctx.From(ctx); id != nil && id.TenantID() == tenantID {
		schema = id.DBSchema
	}
	return tenantdb.ReadTenant(ctx, pool, tenantID, schema, sql, args, fn)
}
