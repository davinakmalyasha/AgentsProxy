// DONE: T1b (tenant allowlist) / STANDARDS A
package records

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// Querier is the QueryRow subset both *pgxpool.Pool and pgx.Tx satisfy.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ErrTenantLookup marks an infrastructure failure during the tenant
// lookup — as opposed to a tenant that genuinely does not exist.
var ErrTenantLookup = errors.New("tenant lookup failed")

// CheckTenant reports whether tenantID belongs to an active client, and
// keeps "no such tenant" separate from "the lookup itself broke".
//
// The distinction matters: this runs before any tenant context exists, so
// a fault here (RLS denying the role, a missing grant, a pool timeout) is
// indistinguishable from an unknown tenant if it is collapsed into a bare
// bool — the caller then answers 400 "unknown tenant" and blames a caller
// for a server-side problem. That is exactly how a policy-less clients
// table went unnoticed: the real cause (row-level security) was reported
// as a bad request. Callers that can distinguish should use this and
// answer 5xx on ErrTenantLookup.
func CheckTenant(ctx context.Context, q Querier, tenantID string) (bool, error) {
	if tenantID == "" {
		return false, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var one int
	err := q.QueryRow(cctx,
		`SELECT 1 FROM clients WHERE tenant_id=$1 AND status='active'`, tenantID).Scan(&one)
	if err != nil {
		// Not-a-client is a normal answer, not a failure.
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("%w: %w", ErrTenantLookup, err)
	}
	return true, nil
}

// KnownTenant reports whether tenantID belongs to an active client.
// Used wherever a tenant arrives from user input (public reads,
// client-chat): arbitrary strings must not become Redis keys, RLS
// contexts, or cross-tenant read paths. Single indexed lookup.
//
// Prefer CheckTenant where the caller can return a 5xx; this wrapper
// keeps the deny-by-default behaviour and only adds logging.
func KnownTenant(ctx context.Context, q Querier, tenantID string) bool {
	ok, err := CheckTenant(ctx, q, tenantID)
	if err != nil {
		// Deny by default, but never silently: an unknown tenant is the
		// caller's problem, this is ours.
		slog.Error("tenant lookup failed", "tenant", tenantID, "err", err)
	}
	return ok
}
