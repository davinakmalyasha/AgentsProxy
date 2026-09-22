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

	"github.com/davinakmalyasha/agents-proxy/internal/auth"
)

const (
	ckTTL          = 900 * time.Second
	cacheKeyPrefix = "ck:"
)

var ErrInvalidToken = errors.New("invalid token")

type Client struct {
	DB  *pgxpool.Pool
	RDB *redis.Client
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
func (c *Client) GetByToken(ctx context.Context, token string) (*ClientModel, error) {
	hash := auth.HashToken(token)
	if m, err := c.fromCache(ctx, hash); err == nil && m != nil {
		return m, nil
	}
	m, err := c.fromDB(ctx, hash)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, ErrInvalidToken
	}
	if err := c.writeCache(ctx, hash, m); err != nil {
		// cache failure is non-fatal
		_ = err
	}
	return m, nil
}

func (c *Client) fromCache(ctx context.Context, hash string) (*ClientModel, error) {
	b, err := c.RDB.Get(ctx, cacheKeyPrefix+hash).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m ClientModel
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
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

// EvictByHash removes a cached client entry. Safe to call when missing.
func (c *Client) EvictByHash(ctx context.Context, hash string) error {
	if hash == "" {
		return nil
	}
	return c.RDB.Del(ctx, cacheKeyPrefix+hash).Err()
}
