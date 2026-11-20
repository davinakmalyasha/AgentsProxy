package db

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestPool_Real(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// migrateDSN returns the DSN to use for migrations. If
// TEST_DATABASE_ADMIN_URL is set (production-style split), use that;
// otherwise fall back to TEST_DATABASE_URL. Tests that need to call
// Migrate() require CREATE EXTENSION/ALTER TABLE privileges.
func migrateDSN() string {
	if v := os.Getenv("TEST_DATABASE_ADMIN_URL"); v != "" {
		return v
	}
	return os.Getenv("TEST_DATABASE_URL")
}

func TestPool_BadDSN(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := NewPool(ctx, "not-a-valid-dsn"); err == nil {
		t.Fatal("expected error for invalid dsn")
	}
}
