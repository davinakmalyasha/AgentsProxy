package features

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/features/clientctx"
	"github.com/davinakmalyasha/agents-proxy/internal/tenanthealth"
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

// RequireTenantHealthy returns middleware that fails closed for
// quarantined tenants: 503 with a distinct body, before any handler
// opens a transaction or burns quota. It runs AFTER identity middleware
// (so unauthenticated callers still get 401 first and cannot probe
// quarantine state) and BEFORE rate-limit and feature gates (so fenced
// traffic neither consumes budget nor masks behind 403s).
//
// Unknown tenants (no identity) pass through: this gate only fences
// explicitly quarantined slugs. Auth handlers answer those.
func RequireTenantHealthy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var tenant string
		if id := clientctx.From(r.Context()); id != nil {
			tenant = id.TenantID()
		}
		if tenant != "" {
			if q, reason := tenanthealth.Check(tenant); q {
				slog.Warn("quarantined tenant blocked from REST", "tenant", tenant, "reason", reason, "path", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":"` + tenanthealth.ResponseBody + `"}`))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
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

// RequireProject rejects a request whose resolved tenant is not the project
// that owns this router. This is the hard boundary for /v1/p/<slug>/...:
// authentication happens first, then the request can only reach the schema
// and handlers of the project named in the URL.
func RequireProject(project string) func(http.Handler) http.Handler {
	want := strings.ToLower(strings.TrimSpace(project))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := clientctx.From(r.Context())
			if id == nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"project identity required"}`))
				return
			}
			if got := strings.ToLower(id.TenantID()); got != want {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"project mismatch"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireProjectFeature is the project-scoped equivalent of RequireFeature.
// The feature lives in the project registry, not the global one, so the
// global AllowedForVertical check does not apply. The project router already
// pinned the tenant and vertical before this gate runs.
func RequireProjectFeature(project, name string, publicRead bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := clientctx.From(r.Context())
			if id == nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"feature not enabled for this client"}`))
				return
			}
			if id.Role == clientctx.RoleOwner || id.Role == clientctx.RoleStaff {
				next.ServeHTTP(w, r)
				return
			}
			if id.Role == "public" && publicRead && r.Method == http.MethodGet {
				next.ServeHTTP(w, r)
				return
			}
			if id.Client == nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"feature not enabled for this client"}`))
				return
			}
			for _, f := range id.Client.Features {
				if f == name {
					next.ServeHTTP(w, r)
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"feature not enabled for this client"}`))
		})
	}
}
