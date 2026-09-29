// Rate-limit bucket tests.
//
// The regression these exist for: the pre-auth limiter used to key on
// sha256(X-Client-Token) / sha256(X-Session-Token). Those headers are
// unverified attacker input at that point in the chain, so any caller
// could mint a fresh bucket per request and the limit was decorative -
// on every route, including /v1/chat and the agent tools whose cost cap
// fails open on a Redis error.
//
// These tests need a real Redis. They skip when REDIS_URL is unset so
// `go test ./...` still works without one.
package middleware

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/redis/go-redis/v9"
)

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("REDIS_URL")
	if addr == "" {
		t.Skip("REDIS_URL not set - skipping integration test")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unavailable at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// uniqueScope keeps parallel packages from sharing bucket keys.
func uniqueScope(prefix string) string {
	return fmt.Sprintf("test:%s:%d", prefix, os.Getpid())
}

func req(remoteAddr string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/chat", nil)
	r.RemoteAddr = remoteAddr
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// The vulnerability itself: rotating an unverified credential header
// used to produce a different bucket every time, so N requests stayed
// under a limit of M regardless of N.
func TestRateLimit_HeaderRotationCannotMintBuckets(t *testing.T) {
	rdb := testRedis(t)
	scope := uniqueScope("rot")

	pass := func() int {
		h := RateLimitScope(rdb, 5, scope)
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
		rec := httptest.NewRecorder()
		h(next).ServeHTTP(rec, req("10.0.0.1:1234", map[string]string{
			"X-Client-Token":  "attacker-token-" + strconv.Itoa(len(rec.Body.String())),
			"X-Session-Token": "attacker-session",
		}))
		return rec.Code
	}

	// Five distinct credential values from one IP.
	first := make([]int, 0, 12)
	for i := 0; i < 5; i++ {
		first = append(first, pass())
	}
	for i, code := range first {
		if code != 200 {
			t.Fatalf("request %d: got %d, want 200 (limit 5 should admit 5)", i, code)
		}
	}

	// Sixth, again with a brand-new token value, must be throttled.
	if code := pass(); code != http.StatusTooManyRequests {
		t.Fatalf("rotating the credential header bypassed the limit: got %d, want 429", code)
	}
}

// Two different IPs must not share a bucket: the limiter is per-caller,
// and the post-auth limiter is what narrows a shared NAT.
func TestRateLimit_DistinctIPsGetDistinctBuckets(t *testing.T) {
	rdb := testRedis(t)
	scope := uniqueScope("ips")
	h := RateLimitScope(rdb, 2, scope)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	rec := httptest.NewRecorder()
	h(next).ServeHTTP(rec, req("10.0.0.1:1111", nil))
	if rec.Code != 200 {
		t.Fatalf("ip .1 first: %d want 200", rec.Code)
	}
	rec = httptest.NewRecorder()
	h(next).ServeHTTP(rec, req("10.0.0.2:2222", nil))
	if rec.Code != 200 {
		t.Fatalf("ip .2 first: %d want 200 (distinct IPs must not share)", rec.Code)
	}

	// Exhaust .1's bucket, .2 must be unaffected.
	for i := 0; i < 2; i++ {
		rec = httptest.NewRecorder()
		h(next).ServeHTTP(rec, req("10.0.0.1:1111", nil))
	}
	rec = httptest.NewRecorder()
	h(next).ServeHTTP(rec, req("10.0.0.2:2222", nil))
	if rec.Code == http.StatusTooManyRequests {
		t.Fatal("exhausting one IP's bucket throttled a different IP")
	}
}

// X-Forwarded-For must not let a caller choose their own bucket.
func TestRateLimit_ForwardedHeaderCannotRotateBuckets(t *testing.T) {
	rdb := testRedis(t)
	scope := uniqueScope("xff")
	h := RateLimitScope(rdb, 3, scope)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	for i := 0; i < 6; i++ {
		rec := httptest.NewRecorder()
		h(next).ServeHTTP(rec, req("10.0.0.9:3333", map[string]string{
			"X-Forwarded-For": fmt.Sprintf("203.0.113.%d", i),
		}))
		want := 200
		if i >= 3 {
			want = http.StatusTooManyRequests
		}
		if rec.Code != want {
			t.Fatalf("request %d: got %d want %d (forwarded header must not affect bucketing)", i, rec.Code, want)
		}
	}
}

// The post-auth limiter keys on the resolved client, so two clients
// behind one IP keep separate budgets - the property the pre-auth
// limiter was wrongly claiming by hashing headers.
func TestRateLimitClient_SeparatesClientsBehindOneIP(t *testing.T) {
	rdb := testRedis(t)
	scope := uniqueScope("client")

	asClient := func(id string) int {
		h := RateLimitClient(rdb, 2, scope)
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
		r := req("10.0.0.7:4444", nil)
		r = r.WithContext(SetClientID(r.Context(), id))
		rec := httptest.NewRecorder()
		h(next).ServeHTTP(rec, r)
		return rec.Code
	}

	for i := 0; i < 2; i++ {
		if c := asClient("client-a"); c != 200 {
			t.Fatalf("client-a request %d: %d want 200", i, c)
		}
	}
	if c := asClient("client-a"); c != http.StatusTooManyRequests {
		t.Fatalf("client-a third request: %d want 429", c)
	}
	// Same IP, different client: must still have budget.
	if c := asClient("client-b"); c != 200 {
		t.Fatalf("client-b was throttled by client-a's exhaustion: %d want 200", c)
	}
}

// A request that reaches an authenticated route with no resolved
// identity must still be bounded, not left unmetered.
func TestRateLimitClient_FallsBackToIPWithoutIdentity(t *testing.T) {
	rdb := testRedis(t)
	scope := uniqueScope("noident")
	h := RateLimitClient(rdb, 2, scope)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	for i := 0; i < 4; i++ {
		rec := httptest.NewRecorder()
		h(next).ServeHTTP(rec, req("10.0.0.8:5555", nil))
		want := 200
		if i >= 2 {
			want = http.StatusTooManyRequests
		}
		if rec.Code != want {
			t.Fatalf("request %d: got %d want %d (must fall back to IP)", i, rec.Code, want)
		}
	}
}

func TestAllowClient_DisablesWhenLimitNotPositive(t *testing.T) {
	rdb := testRedis(t)
	ok, _ := AllowClient(context.Background(), rdb, 0, uniqueScope("off"), "c1")
	if !ok {
		t.Error("perMinute=0 must disable the check")
	}
	ok, _ = AllowClient(context.Background(), rdb, -1, uniqueScope("off"), "c1")
	if !ok {
		t.Error("negative perMinute must disable the check")
	}
	ok, _ = AllowClient(context.Background(), rdb, 10, uniqueScope("off"), "")
	if !ok {
		t.Error("empty clientID must not consume a bucket")
	}
}

// Nil Redis must leave the route open rather than panicking: the
// unit-test harnesses construct BuilderDeps without one.
func TestRateLimit_NilRedisIsPassthrough(t *testing.T) {
	called := false
	h := RateLimitClient(nil, 1, "x")
	rec := httptest.NewRecorder()
	h(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	})).ServeHTTP(rec, req("10.0.0.1:1", nil))
	if !called {
		t.Error("nil Redis must pass the request through, not block it")
	}
}

func TestRateLimit_RetryAfterIsPositive(t *testing.T) {
	rdb := testRedis(t)
	scope := uniqueScope("retryafter")
	h := RateLimitScope(rdb, 1, scope)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	rec := httptest.NewRecorder()
	h(next).ServeHTTP(rec, req("10.0.0.10:6000", nil))
	rec = httptest.NewRecorder()
	h(next).ServeHTTP(rec, req("10.0.0.10:6000", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429", rec.Code)
	}
	if v := rec.Header().Get("Retry-After"); v == "" {
		t.Error("429 must carry Retry-After")
	} else if n, err := strconv.Atoi(v); err != nil || n < 0 || n > 60 {
		t.Errorf("Retry-After=%q out of range 0..60", v)
	}
}
