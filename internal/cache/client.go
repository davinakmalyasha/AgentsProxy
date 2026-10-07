// DONE: PRD 5 (ck:{hash} cache, 15m TTL) / STANDARDS A, B, E
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"github.com/davinakmalyasha/agents-proxy/internal/auth"
)

const (
	ckTTL          = 900 * time.Second
	ckNegativeTTL  = 60 * time.Second
	cacheKeyPrefix = "ck:"
)

// negSentinel marks a known-invalid token. An invalid token otherwise
// hits Postgres on EVERY request — an unbounded auth-failure amplifier.
const negSentinel = "-"

var ErrInvalidToken = errors.New("invalid token")

type Client struct {
	DB  *pgxpool.Pool
	RDB *redis.Client
	sf  singleflight.Group
}

type ClientModel struct {
	ID                 string   `json:"id"`
	TenantID           string   `json:"tenant_id"`
	Status             string   `json:"status"`
	MonthlyQuotaTokens int      `json:"monthly_quota_tokens"`
	Name               string   `json:"name"`
	TokenHash          string   `json:"token_hash"`
	Vertical           string   `json:"vertical"`
	Features           []string `json:"features"`
}

// GetByToken returns the client for the given raw token, going
// ck:{sha256} → DB. Returns ErrInvalidToken if not found.
//
// Concurrent misses for one token collapse to a single DB query
// (singleflight): without it every 15-minute TTL boundary stampedes
// Postgres once per in-flight request. Invalid tokens are cached
// negative for 60s; EvictByHash clears both on provision/change.
func (c *Client) GetByToken(ctx context.Context, token string) (*ClientModel, error) {
	hash := auth.HashToken(token)
	v, err, _ := c.sf.Do(hash, func() (any, error) {
		return c.loadByHash(ctx, hash)
	})
	if err != nil {
		return nil, err
	}
	return v.(*ClientModel), nil
}

func (c *Client) loadByHash(ctx context.Context, hash string) (*ClientModel, error) {
	if m, negative, err := c.fromCache(ctx, hash); err == nil {
		if negative {
			return nil, ErrInvalidToken
		}
		return m, nil
	}
	m, err := c.fromDB(ctx, hash)
	if err != nil {
		return nil, err
	}
	if m == nil {
		// Negative cache: absorb invalid-token floods. Best-effort;
		// a failed write just means the next attempt re-queries.
		_ = c.RDB.Set(ctx, cacheKeyPrefix+hash, negSentinel, ckNegativeTTL).Err()
		return nil, ErrInvalidToken
	}
	// cache failure is non-fatal: serve from DB without caching.
	_ = c.writeCache(ctx, hash, m)
	return m, nil
}

// fromCache returns (model, false, nil) on a hit, (nil, true, nil) on
// a negative sentinel, and (nil, false, err) on miss or failure.
func (c *Client) fromCache(ctx context.Context, hash string) (*ClientModel, bool, error) {
	b, err := c.RDB.Get(ctx, cacheKeyPrefix+hash).Bytes()
	if err == redis.Nil {
		return nil, false, err
	}
	if err != nil {
		return nil, false, err
	}
	if string(b) == negSentinel {
		return nil, true, nil
	}
	var m ClientModel
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, false, err
	}
	return &m, false, nil
}

func (c *Client) writeCache(ctx context.Context, hash string, m *ClientModel) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return c.RDB.Set(ctx, cacheKeyPrefix+hash, b, ckTTL).Err()
}

func (c *Client) fromDB(ctx context.Context, hash string) (*ClientModel, error) {
	var m ClientModel
	err := c.DB.QueryRow(ctx,
		`SELECT id::text, tenant_id, status, monthly_quota_tokens, name, token_hash, vertical, features
		   FROM clients WHERE token_hash = $1`,
		hash,
	).Scan(&m.ID, &m.TenantID, &m.Status, &m.MonthlyQuotaTokens, &m.Name, &m.TokenHash, &m.Vertical, &m.Features)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query client: %w", err)
	}
	return &m, nil
}

// RDBOf returns c's Redis client, nil-safe. Identity and tenant
// lookups need the raw client for small caches; callers must handle
// a nil Redis (CheckTenantCached and friends skip caching then).
func RDBOf(c *Client) *redis.Client {
	if c == nil {
		return nil
	}
	return c.RDB
}

// EvictByHash removes a cached client entry. Safe to call when missing.
func (c *Client) EvictByHash(ctx context.Context, hash string) error {
	if hash == "" {
		return nil
	}
	return c.RDB.Del(ctx, cacheKeyPrefix+hash).Err()
}
