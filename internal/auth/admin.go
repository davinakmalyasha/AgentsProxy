// DONE: PRD 6.3 (RequireAdmin middleware) / STANDARDS A, D
package auth

import (
	"net/http"
	"strings"
)

// RequireAdmin validates Authorization: Bearer <ADMIN_API_KEY>.
// On miss, writes 401 and aborts.
func RequireAdmin(adminKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			if !strings.HasPrefix(h, "Bearer ") {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			got := strings.TrimPrefix(h, "Bearer ")
			if !Equal(got, adminKey) {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
