// DONE: PRD 5 (Redis INCR+EXPIRE 60s) / STANDARDS D, E
package middleware

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RateLimit enforces per-client (or per-IP) request-per-minute.
// Uses INCR + EXPIRE-on-first-hit. Bypassed when limit <= 0.
// NOTE: nested RateLimit middlewares used to share ONE bucket key, so a
// chat POST counted twice and landing polls ate the chat quota (first
// message OK, follow-ups 429 → hardcoded fallback). Use RateLimitScope
// for nested groups so each group gets its own bucket.
func RateLimit(rdb *redis.Client, perMinute int) func(http.Handler) http.Handler {
	return RateLimitScope(rdb, perMinute, "global")
}

// RateLimitScope is RateLimit with an isolated bucket namespace per scope.
func RateLimitScope(rdb *redis.Client, perMinute int, scope string) func(http.Handler) http.Handler {
	if perMinute <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := rateKey(r)
			now := time.Now().UTC()
			bucket := now.Format("200601021504") // YYYYMMDDHHmm
			redisKey := fmt.Sprintf("rl:%s:%s:%s", scope, key, bucket)
			ctx := r.Context()
			n, err := rdb.Incr(ctx, redisKey).Result()
			if err != nil {
				// Fail open: never block traffic on Redis hiccup.
				next.ServeHTTP(w, r)
				return
			}
			if n == 1 {
				_ = rdb.Expire(ctx, redisKey, 65*time.Second).Err()
			}
			if int(n) > perMinute {
				sec := 60 - now.Second()
				w.Header().Set("Retry-After", strconv.Itoa(sec))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":"rate limit exceeded"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func rateKey(r *http.Request) string {
	if cid, ok := r.Context().Value(CtxClientID).(string); ok && cid != "" {
		return "c:" + cid
	}
	// Credential-aware key (the CtxClientID branch above is dead in
	// practice: auth middleware runs AFTER the limiter). One account
	// behind NAT no longer shares a bucket with strangers, and rotating
	// IPs no longer multiplies an attacker's quota for authed routes.
	// Anon traffic still keys by IP below.
	if tok := strings.TrimSpace(r.Header.Get("X-Session-Token")); tok != "" {
		return "t:" + shortHash(tok)
	}
	if tok := strings.TrimSpace(r.Header.Get("X-Client-Token")); tok != "" {
		return "t:" + shortHash(tok)
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	return "ip:" + ip
}

// shortHash identifies a credential for bucketing without storing it.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}
