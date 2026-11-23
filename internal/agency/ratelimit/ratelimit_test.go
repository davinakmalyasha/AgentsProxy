package ratelimit

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_URL")
	if addr == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(t.Context()).Err(); err != nil {
		t.Skipf("redis unavailable: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func TestLimitPerTrustedIP(t *testing.T) {
	rdb := testRedis(t)
	scope := fmt.Sprintf("test-%d-%d", os.Getpid(), time.Now().UnixNano())
	l := NewRateLimiter(rdb, 2, time.Minute, scope)
	h := l.Limit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	pass := func(remote, xff string) int {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}

	for i := 0; i < 2; i++ {
		if code := pass("127.0.0.1:1111", "203.0.113.1"); code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i, code)
		}
	}
	if code := pass("127.0.0.1:1111", "203.0.113.1"); code != http.StatusTooManyRequests {
		t.Fatalf("third request = %d, want 429", code)
	}
	if code := pass("127.0.0.1:1111", "203.0.113.2"); code != http.StatusOK {
		t.Fatalf("different client IP = %d, want 200", code)
	}

	// An untrusted direct peer cannot rotate X-Forwarded-For to mint a new
	// bucket; it must stay on its own socket address.
	for i := 0; i < 2; i++ {
		if code := pass("203.0.113.9:2222", fmt.Sprintf("198.51.100.%d", i)); code != http.StatusOK {
			t.Fatalf("direct peer request %d = %d, want 200", i, code)
		}
	}
	if code := pass("203.0.113.9:2222", "198.51.100.99"); code != http.StatusTooManyRequests {
		t.Fatalf("spoofed XFF escaped the socket bucket: got %d, want 429", code)
	}
}
