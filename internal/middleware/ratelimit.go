// DONE: PRD 5 (Redis INCR+EXPIRE 60s) / STANDARDS D, E
package middleware

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Rate limiting comes in two halves, and the split is the whole point:
//
//   - RateLimit (this file's exported middleware) runs BEFORE auth,
//     because auth needs a live Redis and a database round trip. At that
//     point no credential has been verified, so the only thing it can
//     safely key on is the caller's IP.
//   - RateLimitClient / AllowClient run AFTER the identity middleware
//     has resolved a client, and key on the verified client id.
//
// Why the pre-auth limiter must not hash a credential header: doing so
// let any caller mint a fresh bucket per request
// (`curl -H "X-Client-Token: $RANDOM"`), because the bucket name is
// derived from a value the caller chose and nothing had validated it.
// That defeats the limit on every route, including /v1/chat and the
// agent tools whose cost cap fails open on a Redis error.
//
// Per-account isolation is not lost by keying on IP first — AllowClient
// restores it once the identity is known.
//
// NOTE: nested RateLimit middlewares share ONE bucket key, so a chat
// POST counted twice and landing polls ate the chat quota (first
// message OK, follow-ups 429 → hardcoded fallback). Use RateLimitScope
// for nested groups so each group gets its own bucket.
func RateLimit(rdb *redis.Client, perMinute int) func(http.Handler) http.Handler {
	return RateLimitScope(rdb, perMinute, "global")
}

// RateLimitScope is RateLimit with an isolated bucket namespace per scope.
func RateLimitScope(rdb *redis.Client, perMinute int, scope string) func(http.Handler) http.Handler {
	return rateLimitBy(rdb, perMinute, scope, rateKey)
}

// RateLimitClient is the per-credential half. Mount it after the
// identity middleware (records.WithIdentity / WithClientAuth), which is
// what populates CtxClientID. Accounts behind one NAT stop sharing a
// bucket, and rotating IPs no longer multiplies an authenticated
// caller's budget.
func RateLimitClient(rdb *redis.Client, perMinute int, scope string) func(http.Handler) http.Handler {
	return rateLimitBy(rdb, perMinute, scope, verifiedClientKey)
}

func rateLimitBy(rdb *redis.Client, perMinute int, scope string, keyFn func(*http.Request) string) func(http.Handler) http.Handler {
	if perMinute <= 0 || rdb == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ok, retryAfter := Allow(r.Context(), rdb, perMinute, scope, keyFn(r))
			if !ok {
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":"rate limit exceeded"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Allow consumes one unit from the caller's bucket in scope and reports
// whether it was within budget, plus the seconds to advertise in
// Retry-After. Split out from the middleware so a handler that resolves
// identity inline (/v1/chat reads the token from the request body) can
// apply the same per-account limit without going through middleware.
//
// Fails open on a Redis error, matching the documented policy: a cache
// outage must never block traffic. perMinute <= 0 disables the check.
func Allow(ctx context.Context, rdb *redis.Client, perMinute int, scope, key string) (ok bool, retryAfter int) {
	if perMinute <= 0 || rdb == nil {
		return true, 0
	}
	now := time.Now().UTC()
	bucket := now.Format("200601021504") // YYYYMMDDHHmm
	redisKey := fmt.Sprintf("rl:%s:%s:%s", scope, key, bucket)

	n, err := rdb.Incr(ctx, redisKey).Result()
	if err != nil {
		return true, 0 // fail open
	}
	if n == 1 {
		_ = rdb.Expire(ctx, redisKey, 65*time.Second).Err()
	}
	if int(n) > perMinute {
		return false, 60 - now.Second()
	}
	return true, 0
}

// AllowClient is Allow for a caller whose identity is already resolved.
func AllowClient(ctx context.Context, rdb *redis.Client, perMinute int, scope, clientID string) (bool, int) {
	if clientID == "" {
		return true, 0
	}
	return Allow(ctx, rdb, perMinute, scope, "c:"+clientID)
}

// rateKey is the PRE-auth bucket key: IP only, by design. See the
// file comment for why a credential header is not acceptable here.
func rateKey(r *http.Request) string {
	if cid, ok := r.Context().Value(CtxClientID).(string); ok && cid != "" {
		return "c:" + cid
	}
	return "ip:" + remoteIP(r)
}

// verifiedClientKey is the POST-auth bucket key. Falls back to IP so a
// request that reached an authenticated route without a resolved
// identity is still bounded rather than unmetered.
func verifiedClientKey(r *http.Request) string {
	if cid, ok := r.Context().Value(CtxClientID).(string); ok && cid != "" {
		return "c:" + cid
	}
	return "ip:" + remoteIP(r)
}

// remoteIP reads the socket peer only. Forwarded headers are NOT
// consulted here: internal/api/clientip.go owns that, including the
// TRUSTED_PROXY_CIDRS check, and duplicating it here would let a caller
// choose their own bucket by sending X-Forwarded-For.
func remoteIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	return ip
}
