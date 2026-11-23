// DONE: PRD 6.3 (RequireAdmin middleware) / STANDARDS A, D
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

type adminActorKey struct{}

// AdminActor returns the audit-safe identity that RequireAdmin attached to
// the request. It is an ADMIN_API_KEY fingerprint, never the key itself.
// Empty means the request did not pass the admin gate.
func AdminActor(ctx context.Context) string {
	actor, _ := ctx.Value(adminActorKey{}).(string)
	return actor
}

// adminFingerprint is stable for one configured key and reveals nothing
// useful about it: 12 hex characters of SHA-256. It distinguishes keys across
// rotations without putting the secret in a database or log line.
func adminFingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "admin-key:" + hex.EncodeToString(sum[:6])
}

// RequireAdmin validates Authorization: Bearer <ADMIN_API_KEY>.
// On miss, writes 401 and aborts.
func RequireAdmin(adminKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if adminKey == "" {
				http.Error(w, `{"error":"admin surface disabled"}`, http.StatusServiceUnavailable)
				return
			}
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
			ctx := context.WithValue(r.Context(), adminActorKey{}, adminFingerprint(adminKey))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
