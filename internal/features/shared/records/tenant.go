// DONE: T1b (tenant allowlist) / STANDARDS A
package records

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
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

// knownTenantKey is the Redis key for the tenant-exists cache. Short
// TTL: a just-provisioned tenant must become visible quickly, and a
// just-paused one must fence quickly. Admin writes evict it eagerly
// (see EvictTenantKnown); the TTL is the backstop, not the mechanism.
const knownTenantKeyPrefix = "tknown:"

// CheckTenantCached is CheckTenant with a 30s Redis cache. The tenant
// lookup sits on the hottest anonymous path (every public read, every
// client-chat); without this, landing traffic is a Postgres query per
// request with a thundering herd every time a client row changes.
func CheckTenantCached(ctx context.Context, rdb *redis.Client, q Querier, tenantID string) (bool, error) {
	if tenantID == "" {
		return false, nil
	}
	if rdb != nil {
		if v, err := rdb.Get(ctx, knownTenantKeyPrefix+tenantID).Result(); err == nil {
			return v == "1", nil
		}
	}
	ok, err := CheckTenant(ctx, q, tenantID)
	if err != nil {
		return false, err
	}
	if rdb != nil {
		val := "0"
		if ok {
			val = "1"
		}
		_ = rdb.Set(ctx, knownTenantKeyPrefix+tenantID, val, 30*time.Second).Err()
	}
	return ok, nil
}

// EvictTenantKnown drops a cached tenant-exists entry. Call on every
// clients write (provision, patch, status change) so the 30s TTL never
// delays a fence or a launch.
func EvictTenantKnown(ctx context.Context, rdb *redis.Client, tenantID string) error {
	if rdb == nil || tenantID == "" {
		return nil
	}
	return rdb.Del(ctx, knownTenantKeyPrefix+tenantID).Err()
}
