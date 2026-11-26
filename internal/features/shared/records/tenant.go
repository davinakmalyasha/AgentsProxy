// DONE: T1b (tenant allowlist) / STANDARDS A
package records

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"github.com/davinakmalyasha/agents-proxy/internal/tenantdb"
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

// tenantInfoLoads collapses concurrent cold-cache misses for one
// (Redis client, query pool, tenant) so a public burst cannot stampede
// Postgres when the 30s entry expires.
var tenantInfoLoads singleflight.Group

// Tenant info is read on every public landing request and every anonymous
// chat turn. Redis is the cross-instance source of truth (30s), but a Redis
// round trip on each request is pure overhead for a value that changes only
// on an admin write. This process-local L1 is deliberately short (5s): an
// admin eviction clears the handling instance immediately and every other
// instance converges within 5s — fresher than the 30s Redis entry it fronts.
// The 30s L2 stays, so a cold instance or an L1 eviction still avoids a DB
// query.
const (
	tenantInfoL1TTL = 5 * time.Second
	tenantInfoL1Max = 4096
)

type tenantInfoL1Entry struct {
	info    TenantInfo
	expires time.Time
}

var (
	tenantInfoL1Mu sync.Mutex
	tenantInfoL1   = map[string]tenantInfoL1Entry{}
)

// tenantInfoCacheKey includes both the Redis client and query pool so tests
// (and a future multi-pool host) cannot share routing state across databases.
func tenantInfoCacheKey(rdb *redis.Client, q Querier, tenantID string) string {
	return fmt.Sprintf("%p|%p|%s", rdb, q, tenantID)
}

func getTenantInfoL1(key string) (TenantInfo, bool) {
	tenantInfoL1Mu.Lock()
	e, ok := tenantInfoL1[key]
	tenantInfoL1Mu.Unlock()
	if !ok || time.Now().After(e.expires) {
		return TenantInfo{}, false
	}
	return e.info, true
}

func putTenantInfoL1(key string, info TenantInfo) {
	now := time.Now()
	tenantInfoL1Mu.Lock()
	if len(tenantInfoL1) >= tenantInfoL1Max {
		for k, e := range tenantInfoL1 {
			if now.After(e.expires) {
				delete(tenantInfoL1, k)
			}
		}
		if len(tenantInfoL1) >= tenantInfoL1Max {
			tenantInfoL1 = map[string]tenantInfoL1Entry{}
		}
	}
	tenantInfoL1[key] = tenantInfoL1Entry{info: info, expires: now.Add(tenantInfoL1TTL)}
	tenantInfoL1Mu.Unlock()
}

// ResetTenantInfoCacheForTest clears process-local tenant routing state.
// Integration tests reuse tenant ids across databases; production never
// calls this (admin writes use EvictTenantKnown).
func ResetTenantInfoCacheForTest() {
	tenantInfoL1Mu.Lock()
	tenantInfoL1 = map[string]tenantInfoL1Entry{}
	tenantInfoL1Mu.Unlock()
}

// CheckTenantInfoCached is LookupTenantInfo with a 30s Redis cache. The
// lookup sits on the hottest anonymous path (every public read, every
// client-chat); without this, landing traffic is a Postgres query per
// request with a thundering herd every time a client row changes.
func CheckTenantInfoCached(ctx context.Context, rdb *redis.Client, q Querier, tenantID string) (TenantInfo, error) {
	if tenantID == "" {
		return TenantInfo{}, nil
	}
	key := tenantInfoCacheKey(rdb, q, tenantID)
	if info, ok := getTenantInfoL1(key); ok {
		return info, nil
	}
	v, err, _ := tenantInfoLoads.Do(key, func() (any, error) {
		// Re-check after waiting on the flight: a concurrent request may
		// have populated L1 while this one was blocked.
		if info, ok := getTenantInfoL1(key); ok {
			return info, nil
		}
		// Detach from the first waiter's cancellation: one client hanging
		// up must not fail every request sharing this cold miss. The
		// lookup itself has a 2s bound; the outer bound covers the Redis
		// read/write around it as well.
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		return checkTenantInfoCachedOnce(lctx, rdb, q, tenantID)
	})
	if err != nil {
		return TenantInfo{}, err
	}
	info, _ := v.(TenantInfo)
	putTenantInfoL1(key, info)
	return info, nil
}

func checkTenantInfoCachedOnce(ctx context.Context, rdb *redis.Client, q Querier, tenantID string) (TenantInfo, error) {
	var info TenantInfo
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
	if tenantID == "" {
		return nil
	}
	tenantdb.Invalidate(tenantID)
	// L1 is keyed by pool/redis pointers; drop every entry for this tenant
	// so a pause/feature change is immediate on the instance that handled
	// the admin write. Other instances converge within tenantInfoL1TTL.
	tenantInfoL1Mu.Lock()
	suffix := "|" + tenantID
	for k := range tenantInfoL1 {
		if strings.HasSuffix(k, suffix) {
			delete(tenantInfoL1, k)
		}
	}
	tenantInfoL1Mu.Unlock()
	if rdb == nil {
		return nil
	}
	return rdb.Del(ctx,
		knownTenantKeyPrefix+tenantID,
		tenantInfoKeyPrefix+tenantID,
	).Err()
}
