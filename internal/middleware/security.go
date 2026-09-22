// Security headers for every API response. Pure additive hardening —
// nothing depends on these headers today, so this cannot break clients;
// it only makes sniffing/framing/caching misbehavior harder.
package middleware

import "net/http"

// SecurityHeaders stamps X-Content-Type-Options, X-Frame-Options,
// Referrer-Policy, Permissions-Policy and Cache-Control on every
// response (including OPTIONS short-circuits handled downstream).
func SecurityHeaders() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
			// API responses carry tokens/session data + one-time report
			// downloads — never let a shared/browser cache serve them.
			h.Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
		})
	}
}
