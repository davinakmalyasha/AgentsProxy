package features

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/davinakmalyasha/agents-proxy/internal/features/clientctx"
	"github.com/davinakmalyasha/agents-proxy/internal/tenanthealth"
)

// DONE: PRD 6.5 (feature gate) / STANDARDS D, F

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

// RequireFeature returns middleware that rejects requests whose principal
// does not have the named feature enabled. Entitlement is tenant-level, so
// staff and public identities are checked too; being inside a tenant does
// not grant a feature the tenant was not provisioned for.
func RequireFeature(name string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := clientctx.From(r.Context())
			if id == nil || !identityHasGlobalFeature(id, name) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"feature not enabled for this client"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequirePlatformVertical pins the platform surface to the tenant's one
// provisioned vertical. Without this, a platform tenant of vertical A could
// call /v1/B/records and write rows under B using its valid token.
func RequirePlatformVertical(vertical string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := clientctx.From(r.Context())
			if id == nil || id.DBSchema != "public" || !strings.EqualFold(id.Vertical, vertical) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"not found"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// identityHasGlobalFeature is the platform feature check. It uses the same
// live entitlement carried on every identity: client token, staff session,
// or public landing read.
func identityHasGlobalFeature(id *clientctx.Identity, name string) bool {
	if id == nil {
		return false
	}
	features, vertical := id.Features, id.Vertical
	if id.Client != nil {
		features, vertical = id.Client.Features, id.Client.Vertical
	}
	return AllowedForVertical(name, vertical) && containsFeature(features, name)
}

func containsFeature(features []string, name string) bool {
	for _, f := range features {
		if f == name {
			return true
		}
	}
	return false
}

// RequirePlatformTenant rejects a request whose resolved tenant is not a
// platform tenant (db_schema='public'). It is the hard boundary for
// /v1/{vertical}/...: a project tenant must never be served by the platform
// surface, even when it shares a vertical with a platform tenant.
func RequirePlatformTenant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := clientctx.From(r.Context())
		if id == nil || id.DBSchema != "public" {
			// This is the retired project-on-platform surface. Answer 404,
			// the same as the old alias, so a project tenant cannot tell
			// whether a platform tenant of this vertical exists.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireProject rejects a request whose resolved tenant is not the project
// that owns this router, or whose tenant schema is not the project schema.
// This is the hard boundary for /v1/p/<slug>/...: authentication happens
// first, then the request can only reach the schema and handlers of the
// project named in the URL.
func RequireProject(project, schema string) func(http.Handler) http.Handler {
	want := strings.ToLower(strings.TrimSpace(project))
	wantSchema := strings.TrimSpace(schema)
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
			if wantSchema == "" || id.DBSchema != wantSchema {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"project schema mismatch"}`))
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
//
// PublicRead means "safe for an unauthenticated landing read"; it does not
// mean "enabled without entitlement". A public GET still requires the
// feature in the tenant's live features list.
func RequireProjectFeature(project, name string, publicRead bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := clientctx.From(r.Context())
			if id == nil {
				writeFeatureDenied(w)
				return
			}
			if id.Role == clientctx.RolePublic && (!publicRead || r.Method != http.MethodGet) {
				writeFeatureDenied(w)
				return
			}
			features := id.Features
			if id.Client != nil {
				features = id.Client.Features
			}
			if !containsFeature(features, name) {
				writeFeatureDenied(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writeFeatureDenied(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":"feature not enabled for this client"}`))
}
