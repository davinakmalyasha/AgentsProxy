package features

import (
	"context"
	"log/slog"
	"net/http"
	"time"

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

// LiveVerticals returns the verticals that have at least one active
// tenant, for mounting routers. Verticals with no tenants serve nothing
// but the generic records shell, so skipping them saves ~90% of boot
// route registrations on a young deployment.
//
// Fail-open by design: on any error (or zero rows) it returns nil and
// the caller mounts every AllowedVertical — identical to the old
// behavior. A fresh database with no tenants yet therefore behaves
// exactly as before.
func LiveVerticals(ctx context.Context, pool *pgxpool.Pool) []string {
	if pool == nil {
		return nil
	}
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rows, err := pool.Query(qctx, `SELECT DISTINCT vertical FROM clients WHERE status='active'`)
	if err != nil {
		return nil
	}
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return nil
		}
		if IsAllowedVertical(v) {
			out = append(out, v)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(out) == 0 {
		return nil
	}
	return out
}

// VerticalsToMount resolves the mount list.
//
// live == nil means the lookup could not run (fail-open: mount every vertical,
// the historical behavior). A non-nil empty slice means the query succeeded
// and no platform tenant exists, so nothing is mounted. A vertical with
// registered custom features also mounts while the legacy project route is on,
// so a feature can never be registered yet unreachable.
func VerticalsToMount(live []string) []string {
	if live == nil {
		if !LegacyProjectRoutes() {
			// In platform-only mode a database failure must not silently
			// resurrect the project-owned /v1/{vertical} alias.
			return []string{}
		}
		return AllowedVerticals
	}
	seen := make(map[string]bool, len(AllowedVerticals))
	out := make([]string, 0, len(AllowedVerticals))
	for _, v := range live {
		if IsAllowedVertical(v) && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	if LegacyProjectRoutes() {
		for _, v := range AllowedVerticals {
			if hasCustomFeatures(v) && !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	return out
}

// LivePlatformVerticals is LiveVerticals restricted to platform tenants
// (db_schema='public'). It returns a non-nil empty slice on success with no
// platform tenants, so VerticalsToMount can distinguish "none exist" from
// "lookup failed". A nil pool or query failure returns nil (fail-open).
func LivePlatformVerticals(ctx context.Context, pool *pgxpool.Pool) []string {
	if pool == nil {
		return nil
	}
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rows, err := pool.Query(qctx,
		`SELECT DISTINCT vertical FROM clients WHERE status='active' AND db_schema='public'`)
	if err != nil {
		return nil
	}
	out := make([]string, 0)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return nil
		}
		if IsAllowedVertical(v) {
			out = append(out, v)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil
	}
	return out
}

// hasCustomFeatures reports whether any registered feature brings its
// own routes for the vertical (as opposed to the generic adapter).
func hasCustomFeatures(vertical string) bool {
	for _, f := range All() {
		if f.Routes != nil && AllowedForVertical(f.Name, vertical) {
			return true
		}
	}
	return false
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
