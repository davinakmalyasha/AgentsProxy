// Package ratelimit is the agency plane's public-write throttle.
//
// It is Redis-backed and keys on the shared trusted client IP: Vercel runs
// many serverless instances, so an in-memory map enforces nothing across
// them, and trusting a raw X-Forwarded-For header lets a caller choose a new
// bucket. The platform limiter and this one must agree on the caller.
package ratelimit

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/agency/response"
	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
	"github.com/davinakmalyasha/agents-proxy/internal/requestip"
)

// RateLimiter allows limit requests per fixed window per client IP.
type RateLimiter struct {
	rdb    *redis.Client
	scope  string
	limit  int
	window time.Duration
}

// NewRateLimiter builds a limiter. A nil Redis or a non-positive limit
// disables it (tests and local shells); production always supplies both.
func NewRateLimiter(rdb *redis.Client, limit int, window time.Duration, scope string) *RateLimiter {
	return &RateLimiter{rdb: rdb, limit: limit, window: window, scope: scope}
}

// Limit wraps a handler with the fixed-window check. Redis errors fail open:
// a public intake/login endpoint must not become unavailable because the
// cache is down, and the platform limiter already codifies that policy.
func (l *RateLimiter) Limit(next http.Handler) http.Handler {
	if l == nil || l.rdb == nil || l.limit <= 0 || l.window <= 0 {
		return next
	}
	windowSecs := int64(l.window / time.Second)
	if windowSecs < 1 {
		windowSecs = 1
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bucket := time.Now().Unix() / windowSecs
		key := "rl:agency:" + l.scope + ":" + requestip.ClientIP(r) + ":" + strconv.FormatInt(bucket, 10)
		n, err := redisx.IncrExpireRun(r.Context(), l.rdb, key, l.window+time.Second)
		if err != nil {
			slog.Warn("agency rate limit redis, failing open", "scope", l.scope, "err", err)
			next.ServeHTTP(w, r)
			return
		}
		if int(n) > l.limit {
			response.TooManyRequests(w, "Too many requests, please try again later.")
			return
		}
		next.ServeHTTP(w, r)
	})
}
