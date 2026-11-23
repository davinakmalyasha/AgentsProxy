package features

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
	"github.com/davinakmalyasha/agents-proxy/internal/authapi"
	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/davinakmalyasha/agents-proxy/internal/middleware"
)

// DONE: PRD 6.5 (vertical builder) / STANDARDS A, D, F

// BuilderDeps is the minimal dependency set the builder needs.
type BuilderDeps struct {
	Pool  *pgxpool.Pool
	Cache *cache.Client
	Auth  *authapi.Auth // staff session resolver (may be nil in tests)
	// Redis + RateLimitPerMinute drive the per-account limiter, which
	// can only run AFTER the identity middleware resolves a client.
	// Both may be zero: a nil Redis leaves the limiter unmounted, which
	// is what the unit-test harnesses rely on.
	Redis              *redis.Client
	RateLimitPerMinute int
}

// AllowedVerticals is re-exported for the router and tests. The single
// source lives in agentconfig so a project folder cannot name a vertical
// that the builder does not mount.
var AllowedVerticals = agentconfig.AllowedVerticals

// IsAllowedVertical reports whether v is one of the allowed verticals.
func IsAllowedVertical(v string) bool {
	return agentconfig.IsAllowedVertical(v)
}

// ProjectSchema is the schema a project slug owns. It is deliberately
// derived, not read from clients.db_schema, so the HTTP boundary and the
// database boundary cannot drift apart.
func ProjectSchema(slug string) string {
	return agentconfig.SchemaForSlug(slug)
}

// ValidProjectSlug reports whether a slug can be mounted as /v1/p/<slug>.
func ValidProjectSlug(slug string) bool {
	return agentconfig.SlugRe.MatchString(slug)
}

// BuildVerticalRouter returns a chi router for one vertical. The
// caller is responsible for /v1/{vertical} prefix.
//
// Layout:
//
//	/v1/{vertical}/records/...        ← records feature (default-on, no gate)
//	/v1/{vertical}/{feature}/...      ← each registered feature, gated
//
// Each feature's request is gated by RequireFeature(name) which
// reads the cached *cache.ClientModel from context — zero DB hit.
func BuildVerticalRouter(deps BuilderDeps, vertical string) http.Handler {
	if !IsAllowedVertical(vertical) {
		panic("features.BuildVerticalRouter: unknown vertical " + vertical)
	}
	recDeps := records.Deps{Pool: deps.Pool, Cache: deps.Cache}
	r := chi.NewRouter()
	if deps.Auth != nil {
		r.Use(records.WithIdentity(records.IdentityDeps{Pool: deps.Pool, Cache: deps.Cache, Auth: deps.Auth}))
	} else {
		r.Use(records.WithClientAuth(recDeps))
	}
	// Platform surface only. A project tenant is served exclusively under
	// /v1/p/<slug>; this gate is what makes that true even when a platform
	// tenant of the same vertical is live.
	r.Use(RequirePlatformTenant)
	r.Use(RequirePlatformVertical(vertical))
	// Quarantine fence: after identity (401 first, no oracle for
	// unauthenticated callers), before rate-limit and feature gates (no
	// quota burn, no 403 masking). Covers /records and every feature.
	r.Use(RequireTenantHealthy)
	// Per-account limiter, mounted AFTER the identity middleware so
	// CtxClientID is populated. The outer /v1 limiter already bounds by
	// IP; this restores the per-client budget that IP bucketing would
	// otherwise merge for everyone behind one NAT. Scope is namespaced
	// per vertical so one tenant's traffic cannot consume another's.
	if deps.Redis != nil && deps.RateLimitPerMinute > 0 {
		r.Use(middleware.RateLimitClient(deps.Redis, deps.RateLimitPerMinute, "client:"+vertical))
	}
	rec := records.New(recDeps)
	// records is always available, never gated.
	r.Mount("/records", rec.Routes(vertical))
	mounted := 0
	for _, f := range All() {
		if f.Name == "records" {
			continue
		}
		if !AllowedForVertical(f.Name, vertical) {
			continue
		}
		gate := RequireFeature(f.Name)
		if f.Routes != nil {
			// Custom feature: it brings its own routes (e.g. a
			// real-table feature with custom handlers). Mount them
			// gated; the feature is responsible for its own
			// withTenantTx / RLS discipline.
			r.Route("/"+f.Name, func(rr chi.Router) {
				rr.Use(gate)
				f.Routes(rr, deps)
			})
			mounted++
			continue
		}
		adapter := records.NewFeatureAdapter(recDeps, records.FeatureSpec{
			Name:            f.Name,
			Kind:            f.Kind,
			ValidatePayload: f.ValidatePayload,
		}, vertical)
		r.Route("/"+f.Name, func(rr chi.Router) {
			rr.Use(gate)
			rr.Mount("/", adapter.Routes())
		})
		mounted++
	}
	// One line per vertical, not per feature: 13 verticals × 18 features
	// used to emit ~90 Info lines on every cold start.
	slog.Debug("vertical mounted", "vertical", vertical, "features", mounted)
	return r
}
