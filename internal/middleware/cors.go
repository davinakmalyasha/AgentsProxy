// DONE: PRD 7 (CORS from ALLOW_ORIGINS) / STANDARDS A, D
package middleware

import (
	"net/http"
)

// CORS emits permissive Access-Control headers from the configured
// allow-list. If "*" is in the list, allows any origin. Credentials
// are always off (per PRD/STANDARDS).
func CORS(allowOrigins []string) func(http.Handler) http.Handler {
	wildcard := false
	for _, o := range allowOrigins {
		if o == "*" {
			wildcard = true
			break
		}
	}
	allowed := make(map[string]struct{}, len(allowOrigins))
	for _, o := range allowOrigins {
		if o != "*" {
			allowed[o] = struct{}{}
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if wildcard {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if origin != "" {
				if _, ok := allowed[origin]; ok {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Vary", "Origin")
				}
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Client-Token, X-Session-Token, X-Request-ID")
			w.Header().Set("Access-Control-Max-Age", "600")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
