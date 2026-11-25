// DONE: PRD 7 (CORS from ALLOW_ORIGINS) / STANDARDS A, D
package middleware

import (
	"net/http"
)

// CORSResolving is CORS plus the DB origin registry (migration 058):
// an origin is allowed when it is in the static list OR the resolver
// allows it. A nil resolver degrades exactly to CORS(staticExtra), so
// tests and local dev never need a database. Static entries stay
// exact-only (no suffix interpretation); suffix rules live in the DB.
func CORSResolving(staticExtra []string, res *DBResolver) func(http.Handler) http.Handler {
	static := CORS(staticExtra)
	if res == nil {
		return static
	}
	return func(next http.Handler) http.Handler {
		staticH := static(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && res.Allowed(origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
			}
			staticH.ServeHTTP(w, r)
		})
	}
}

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
			} else {
				// Always advertise the dimension, even for a request with
				// no Origin or a disallowed one. Otherwise a shared cache
				// can store a no-ACAO response and replay it to an allowed
				// origin, or vice versa.
				addVary(w.Header(), "Origin")
				if origin != "" {
					if _, ok := allowed[origin]; ok {
						w.Header().Set("Access-Control-Allow-Origin", origin)
					}
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
