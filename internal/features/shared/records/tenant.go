// DONE: T1b (tenant allowlist) / STANDARDS A
package records

import (
	"context"
	"encoding/json"
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

// TenantInfo is the routing identity of an active tenant. The schema is the
// value the route gates and tenant transactions must agree on.
type TenantInfo struct {
	Active   bool     `json:"active"`
	Schema   string   `json:"schema"`
	Vertical string   `json:"vertical"`
	Features []string `json:"features"`
}

// LookupTenantInfo reports an active client's schema and vertical. A missing
// or non-active tenant returns Active=false, not an error.
func LookupTenantInfo(ctx context.Context, q Querier, tenantID string) (TenantInfo, error) {
	var info TenantInfo
	if tenantID == "" {
		return info, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	err := q.QueryRow(cctx,
		`SELECT db_schema, vertical, features FROM clients WHERE tenant_id=$1 AND status='active'`,
		tenantID).Scan(&info.Schema, &info.Vertical, &info.Features)
	if errors.Is(err, pgx.ErrNoRows) {
		return info, nil
	}
	if err != nil {
		return info, fmt.Errorf("%w: %w", ErrTenantLookup, err)
	}
	info.Active = true
	return info, nil
}

// CheckTenant reports whether tenantID belongs to an active client. It is the
// boolean view of LookupTenantInfo.
func CheckTenant(ctx context.Context, q Querier, tenantID string) (bool, error) {
	info, err := LookupTenantInfo(ctx, q, tenantID)
	return info.Active, err
}

// knownTenantKey is the Redis key for the tenant-exists cache. Short
// TTL: a just-provisioned tenant must become visible quickly, and a
// just-paused one must fence quickly. Admin writes evict it eagerly
// (see EvictTenantKnown); the TTL is the backstop, not the mechanism.
const (
	knownTenantKeyPrefix = "tknown:"
	tenantInfoKeyPrefix  = "tinfo:v2:"
)

// CheckTenantInfoCached is LookupTenantInfo with a 30s Redis cache. The
// lookup sits on the hottest anonymous path (every public read, every
// client-chat); without this, landing traffic is a Postgres query per
// request with a thundering herd every time a client row changes.
func CheckTenantInfoCached(ctx context.Context, rdb *redis.Client, q Querier, tenantID string) (TenantInfo, error) {
	var info TenantInfo
	if tenantID == "" {
		return info, nil
	}
	if rdb != nil {
		if b, err := rdb.Get(ctx, tenantInfoKeyPrefix+tenantID).Bytes(); err == nil {
			if json.Unmarshal(b, &info) == nil {
				return info, nil
			}
		}
	}
	info, err := LookupTenantInfo(ctx, q, tenantID)
	if err != nil {
		return info, err
	}
	if rdb != nil {
		if b, err := json.Marshal(info); err == nil {
			_ = rdb.Set(ctx, tenantInfoKeyPrefix+tenantID, b, 30*time.Second).Err()
		}
	}
	return info, nil
}

// CheckTenantCached is CheckTenant with a 30s Redis cache.
func CheckTenantCached(ctx context.Context, rdb *redis.Client, q Querier, tenantID string) (bool, error) {
	info, err := CheckTenantInfoCached(ctx, rdb, q, tenantID)
	return info.Active, err
}

// EvictTenantKnown drops the cached tenant entries. Call on every clients
// write (provision, patch, status change) so the 30s TTL never delays a
// fence or a launch.
func EvictTenantKnown(ctx context.Context, rdb *redis.Client, tenantID string) error {
	if rdb == nil || tenantID == "" {
		return nil
	}
	return rdb.Del(ctx,
		knownTenantKeyPrefix+tenantID,
		tenantInfoKeyPrefix+tenantID,
	).Err()
}
