package quota

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/auth"
	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
)

func setup(t *testing.T) (context.Context, *db.Pool, *redis.Client, string) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	rurl := os.Getenv("REDIS_URL")
	if dsn == "" || rurl == "" {
		t.Skip("DATABASE_URL / REDIS_URL not set — skipping integration test")
	}
	// Migrations need DDL perms; use admin DSN if set.
	migDSN := os.Getenv("DATABASE_ADMIN_URL")
	if migDSN == "" {
		migDSN = dsn
	}
	// quota tests don't exercise RLS — use the admin DSN for the
	// runtime pool so TRUNCATE/INSERT on `clients` works.
	dsn = migDSN
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	migPool, err := db.NewPool(ctx, migDSN)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, migPool); err != nil {
		migPool.Close()
		t.Fatal(err)
	}
	migPool.Close()
	pool, err := db.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// Targeted cleanup — TRUNCATE would deadlock with parallel tests.
	if _, err := pool.Exec(ctx, `DELETE FROM clients WHERE tenant_id = 'qtest'`); err != nil {
		t.Fatal(err)
	}
	rdb, err := redisx.NewClient(ctx, rurl)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rdb.Close() })

	// Seed client.
	tok := "quota-test-token"
	hash := auth.HashToken(tok)
	if _, err := pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status) VALUES ('qtest','qtest',$1,$2,'active')`,
		hash, 1000,
	); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM clients WHERE token_hash=$1`, hash).Scan(&id); err != nil {
		t.Fatal(err)
	}
	// Clear any leftover cache key.
	rdb.Del(ctx, "quota:"+id+":"+time.Now().UTC().Format("200601"))
	return ctx, pool, rdb, id
}

func TestCheck_UnderLimit(t *testing.T) {
	ctx, pool, rdb, id := setup(t)
	remaining, resetAt, err := Check(ctx, pool, rdb, id, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 1000 {
		t.Errorf("remaining=%d want 1000", remaining)
	}
	if resetAt.IsZero() {
		t.Error("resetAt zero")
	}
}

func TestCheck_OverLimit(t *testing.T) {
	ctx, pool, rdb, id := setup(t)
	// Seed usage that already exceeds quota.
	if _, err := pool.Exec(ctx, `INSERT INTO usage (client_id, tokens_in, tokens_out, model, kind) VALUES ($1, 800, 300, 'm', 'chat')`, id); err != nil {
		t.Fatal(err)
	}
	// Clear cache so SUM recomputes.
	monthKey := time.Now().UTC().Format("200601")
	rdb.Del(ctx, "quota:"+id+":"+monthKey)
	_, resetAt, err := Check(ctx, pool, rdb, id, 1000)
	if !IsExceeded(err) {
		t.Fatalf("err=%v want QuotaExceeded", err)
	}
	if !resetAt.After(time.Now()) {
		t.Errorf("resetAt=%v should be in future", resetAt)
	}
}

func TestCheck_CacheHit(t *testing.T) {
	ctx, pool, rdb, id := setup(t)
	monthKey := time.Now().UTC().Format("200601")
	rdb.Set(ctx, "quota:"+id+":"+monthKey, 500, time.Minute)
	remaining, _, err := Check(ctx, pool, rdb, id, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 500 {
		t.Errorf("remaining=%d want 500", remaining)
	}
}

// TestCheck_CorruptCacheFallsBackToDB: a garbage cached value must never
// read as zero usage. The DB SUM wins.
func TestCheck_CorruptCacheFallsBackToDB(t *testing.T) {
	ctx, pool, rdb, id := setup(t)
	if _, err := pool.Exec(ctx, `INSERT INTO usage (client_id, tokens_in, tokens_out, model, kind) VALUES ($1, 700, 100, 'm', 'chat')`, id); err != nil {
		t.Fatal(err)
	}
	monthKey := time.Now().UTC().Format("200601")
	rdb.Set(ctx, "quota:"+id+":"+monthKey, "garbage!!", time.Minute)
	remaining, _, err := Check(ctx, pool, rdb, id, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 200 {
		t.Errorf("remaining=%d want 200 (800 used from DB, not 0 from corrupt cache)", remaining)
	}
}

// TestCheck_NonPositiveLimitDenies: quota math must never grant on a
// zero/negative limit.
func TestCheck_NonPositiveLimitDenies(t *testing.T) {
	ctx, pool, rdb, id := setup(t)
	if _, _, err := Check(ctx, pool, rdb, id, 0); !IsExceeded(err) {
		t.Errorf("limit 0 err=%v want QuotaExceeded", err)
	}
	if _, _, err := Check(ctx, pool, rdb, id, -5); !IsExceeded(err) {
		t.Errorf("limit -5 err=%v want QuotaExceeded", err)
	}
}
