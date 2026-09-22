package features

import (
	"net/http"

	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/features/clientctx"
)

// DONE: PRD 6.5 (feature gate) / STANDARDS D, F

// HasFeatureOnClient reports whether the named feature is in the
// client's `features` list AND the feature is allowed for the
// client's vertical.
func HasFeatureOnClient(name string, m *cache.ClientModel) bool {
	if m == nil {
		return false
	}
	if !AllowedForVertical(name, m.Vertical) {
		return false
	}
	for _, f := range m.Features {
		if f == name {
			return true
		}
	}
	return false
}

// RequireFeature returns middleware that rejects requests whose
// client (in ctx) does not have the named feature enabled. 403 with
// a stable JSON error. Staff identities (session token) bypass the
// feature gate — they're already inside the tenant.
func RequireFeature(name string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := clientctx.From(r.Context())
			if id == nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"feature not enabled for this client"}`))
				return
			}
			// Staff users (owner/pegawai) are inside the tenant already.
			if id.Role != clientctx.RoleClient {
				next.ServeHTTP(w, r)
				return
			}
			if !HasFeatureOnClient(name, id.Client) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"feature not enabled for this client"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
