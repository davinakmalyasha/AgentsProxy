// DONE: T1b (tenant allowlist) / STANDARDS A
package records

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Querier is the QueryRow subset both *pgxpool.Pool and pgx.Tx satisfy.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// KnownTenant reports whether tenant_id belongs to an active client.
// Used wherever a tenant arrives from user input (public reads,
// client-chat): arbitrary strings must not become Redis keys, RLS
// contexts, or cross-tenant read paths. Single indexed lookup.
func KnownTenant(ctx context.Context, q Querier, tenantID string) bool {
	if tenantID == "" {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var one int
	if err := q.QueryRow(cctx,
		`SELECT 1 FROM clients WHERE tenant_id=$1 AND status='active'`, tenantID).Scan(&one); err != nil {
		return false
	}
	return true
}
