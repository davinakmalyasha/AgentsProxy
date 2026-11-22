package usage

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/auth"
	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
)

func setup(t *testing.T) (context.Context, *db.Pool, *redis.Client) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	rurl := os.Getenv("TEST_REDIS_URL")
	if dsn == "" || rurl == "" {
		t.Skip("TEST_DATABASE_URL / TEST_REDIS_URL not set")
	}
	migDSN := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if migDSN == "" {
		migDSN = dsn
	}
	// usage tests don't exercise RLS — use admin DSN for runtime.
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
	if _, err := pool.Exec(ctx, `DELETE FROM clients WHERE tenant_id = 'u'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM usage WHERE client_id IN (SELECT id FROM clients WHERE tenant_id = 'u')`); err != nil {
		t.Fatal(err)
	}
	rdb, err := redisx.NewClient(ctx, rurl)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rdb.Close() })

	tok := "usage-tok"
	hash := auth.HashToken(tok)
	if _, err := pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status) VALUES ('u','u',$1,1000,'active')`,
		hash,
	); err != nil {
		t.Fatal(err)
	}
	return ctx, pool, rdb
}

func TestRecord_InsertsRow(t *testing.T) {
	ctx, pool, rdb := setup(t)
	// Use the dedicated 'u' client we just seeded.
	var id string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM clients WHERE tenant_id='u'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := Record(ctx, pool, rdb, id, "m", "chat", 10, 20); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM usage WHERE client_id=$1 AND kind='chat'`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("rows=%d", n)
	}
}

func TestRecord_UpdatesQuotaCache(t *testing.T) {
	ctx, pool, rdb := setup(t)
	var id string
	pool.QueryRow(ctx, `SELECT id::text FROM clients LIMIT 1`).Scan(&id)
	rdb.Del(ctx, "quota:"+id+":"+time.Now().UTC().Format("200601"))
	if err := Record(ctx, pool, rdb, id, "m", "chat", 30, 70); err != nil {
		t.Fatal(err)
	}
	v, _ := rdb.Get(ctx, "quota:"+id+":"+time.Now().UTC().Format("200601")).Int()
	if v != 100 {
		t.Errorf("cache=%d want 100", v)
	}
}

func TestRecord_SetsQuotaCacheTTL(t *testing.T) {
	ctx, pool, rdb := setup(t)
	var id string
	pool.QueryRow(ctx, `SELECT id::text FROM clients LIMIT 1`).Scan(&id)
	key := "quota:" + id + ":" + time.Now().UTC().Format("200601")
	rdb.Del(ctx, key)
	if err := Record(ctx, pool, rdb, id, "m", "chat", 10, 10); err != nil {
		t.Fatal(err)
	}
	ttl, err := rdb.TTL(ctx, key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 0 || ttl > 32*24*time.Hour {
		t.Errorf("ttl=%v want a bounded positive expiry", ttl)
	}
}

func toRuntimeDSN(dsn string) string {
	i := strings.Index(dsn, "://")
	if i < 0 {
		return dsn
	}
	rest := dsn[i+3:]
	at := strings.Index(rest, "@")
	if at < 0 {
		return dsn
	}
	return dsn[:i+3] + "app_runtime" + rest[at:]
}

// TestRecord_UsesClientGUC proves the metering path works as app_runtime
// with the scoped usage_write_runtime policy. Without app.client_id the
// insert is rejected, so this fails if Record ever stops setting it.
func TestRecord_UsesClientGUC(t *testing.T) {
	ctx, admin, rdb := setup(t)
	var id string
	if err := admin.QueryRow(ctx, `SELECT id::text FROM clients WHERE tenant_id='u'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	runtime, err := db.NewPool(ctx, toRuntimeDSN(os.Getenv("TEST_DATABASE_URL")))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := Record(ctx, runtime, rdb, id, "m", "chat", 10, 20); err != nil {
		t.Fatalf("Record as app_runtime: %v", err)
	}
	var n int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM usage WHERE client_id=$1 AND kind='chat'`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("usage rows=%d, want 1", n)
	}
}
