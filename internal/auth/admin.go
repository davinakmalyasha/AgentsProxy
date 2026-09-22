// DONE: PRD 6.3 (RequireAdmin middleware) / STANDARDS A, D
package auth

import (
	"context"
	"net/http"
	"strings"
)

type ctxKey int

const (
	ctxAdminOK ctxKey = iota
)

// RequireAdmin validates Authorization: Bearer <ADMIN_API_KEY>.
// On success, marks context with admin=ok; on miss, writes 401 and aborts.
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
			ctx := context.WithValue(r.Context(), ctxAdminOK, true)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func IsAdmin(ctx context.Context) bool {
	v, _ := ctx.Value(ctxAdminOK).(bool)
	return v
}
