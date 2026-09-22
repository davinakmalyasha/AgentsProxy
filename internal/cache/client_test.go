package cache

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/auth"
	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
)

func setup(t *testing.T) (*Client, *redis.Client, context.Context, func()) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	rurl := os.Getenv("REDIS_URL")
	if dsn == "" || rurl == "" {
		t.Skip("DATABASE_URL / REDIS_URL not set — skipping integration test")
	}
	migDSN := os.Getenv("DATABASE_ADMIN_URL")
	if migDSN == "" {
		migDSN = dsn
	}
	// cache tests don't exercise RLS — use the admin DSN for the
	// runtime pool so TRUNCATE/INSERT on `clients` works. The
	// app-layer api tests do verify RLS by connecting as `app`.
	dsn = migDSN
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	migPool, err := db.NewPool(ctx, migDSN)
	if err != nil {
		cancel()
		t.Fatalf("mig NewPool: %v", err)
	}
	if err := db.Migrate(ctx, migPool); err != nil {
		migPool.Close()
		cancel()
		t.Fatalf("Migrate: %v", err)
	}
	migPool.Close()
	pool, err := db.NewPool(ctx, dsn)
	if err != nil {
		cancel()
		t.Fatalf("NewPool: %v", err)
	}
	rdb, err := redisx.NewClient(ctx, rurl)
	if err != nil {
		pool.Close()
		cancel()
		t.Fatalf("redisx: %v", err)
	}
	// Wipe only this test's clients (preserve schema). Use
	// targeted DELETE so we don't deadlock with parallel tests
	// that own other rows.
	if _, err := pool.Exec(ctx, `DELETE FROM clients WHERE tenant_id IN ('acme','globex','initech')`); err != nil {
		pool.Close()
		rdb.Close()
		cancel()
		t.Fatalf("delete: %v", err)
	}
	return &Client{DB: pool, RDB: rdb}, rdb, ctx, func() {
		pool.Close()
		rdb.Close()
		cancel()
	}
}

func TestGetByToken_MissDBHitCache(t *testing.T) {
	c, rdb, ctx, done := setup(t)
	defer done()
	tok := "test-token-1"
	hash := auth.HashToken(tok)
	if _, err := c.RDB.Del(ctx, "ck:"+hash).Result(); err != nil && !errors.Is(err, redis.Nil) {
		t.Fatal(err)
	}
	// Seed a client row.
	if _, err := c.DB.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status) VALUES ($1,$2,$3,$4,$5)`,
		"acme", "acme", hash, 1000, "active",
	); err != nil {
		t.Fatal(err)
	}
	// First call: miss → DB → cache populate.
	m, err := c.GetByToken(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if m.TenantID != "acme" {
		t.Errorf("tenant_id=%s", m.TenantID)
	}
	// Second call: cache hit.
	exists, _ := rdb.Exists(ctx, "ck:"+hash).Result()
	if exists != 1 {
		t.Errorf("ck key not populated; exists=%d", exists)
	}
	m2, err := c.GetByToken(ctx, tok)
	if err != nil || m2.TenantID != "acme" {
		t.Errorf("cached lookup failed: %v %+v", err, m2)
	}
}

func TestGetByToken_NotFound(t *testing.T) {
	c, _, ctx, done := setup(t)
	defer done()
	_, err := c.GetByToken(ctx, "no-such-token")
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("err=%v want ErrInvalidToken", err)
	}
}

func TestEvictByHash(t *testing.T) {
	c, rdb, ctx, done := setup(t)
	defer done()
	tok := "evict-me"
	hash := auth.HashToken(tok)
	// Seed cache.
	if err := rdb.Set(ctx, "ck:"+hash, `{"id":"x"}`, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if err := c.EvictByHash(ctx, hash); err != nil {
		t.Fatal(err)
	}
	n, _ := rdb.Exists(ctx, "ck:"+hash).Result()
	if n != 0 {
		t.Errorf("key still exists after evict: %d", n)
	}
}
