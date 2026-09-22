package redisx

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestClient_Real(t *testing.T) {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set — skipping integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := NewClient(ctx, url)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestClient_LaragonBare(t *testing.T) {
	// Laragon convention: REDIS_URL=localhost:6379 (no scheme)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := NewClient(ctx, "localhost:6379")
	if err != nil {
		t.Skipf("redis not reachable (ok in CI): %v", err)
	}
	defer c.Close()
}
