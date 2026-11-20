// Package records is the shared "records" feature — the generic CRUD
// engine over tenant_records. It also provides the per-request auth
// helper that every other shared feature uses.
//
// DONE: PRD 6.5 (records feature) / STANDARDS A, D, F
package records

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/features/clientctx"
	"github.com/davinakmalyasha/agents-proxy/internal/middleware"
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
//
// A deadlock is retried. It is a normal race, not a malformed request: two
// staff booking the same slot at the same instant both insert, and because
// these tables carry an RLS policy, Postgres cannot satisfy the partial
// EXCLUDE constraint from the index — it re-checks with a heap scan that takes
// row locks, so the two transactions end up waiting on each other and
// PostgreSQL aborts one (SQLSTATE 40P01). Before this, that surfaced to the
// caller as a 500 instead of the 409 the exclusion constraint exists to give.
//
// Retrying is safe and is the point: the constraint is what guarantees only one
// writer can win, so a retry re-runs the same work and settles on success or a
// clean conflict. The aborted transaction rolled back, so fn starts from
// nothing. Only 40P01 is retried — every other error is the caller's answer.
func withTenantTx(ctx context.Context, pool *pgxpool.Pool, tenantID string, fn func(pgx.Tx) error) error {
	const attempts = 3
	for attempt := 1; ; attempt++ {
		err := withTenantTxOnce(ctx, pool, tenantID, fn)
		var deadlocked *pgconn.PgError
		if attempt >= attempts || !errors.As(err, &deadlocked) || deadlocked.Code != "40P01" {
			return err
		}
		// Back off a little so the loser of the race is not immediately
		// handed the win, and stop if the caller gave up meanwhile.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * 20 * time.Millisecond):
		}
	}
}

func withTenantTxOnce(ctx context.Context, pool *pgxpool.Pool, tenantID string, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
	}()
	// The schema lookup is part of the same statement as the GUC set, so a
	// tenant transaction costs one round trip. `clients` is resolved before
	// search_path changes; subsequent statements see only the project schema
	// (or public for legacy Centrum). Excluding public for project schemas is
	// deliberate: an unqualified table that is missing from the project
	// schema must fail, not silently fall back to a shared platform table.
	if _, err := tx.Exec(ctx, `
		SELECT set_config('app.tenant_id', $1, true),
		       set_config('search_path', CASE WHEN s = 'public' THEN 'public' ELSE s END, true)
		  FROM (SELECT COALESCE((SELECT db_schema FROM clients WHERE tenant_id = $1), 'public') AS s) q`,
		tenantID); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("set tenant context: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}
