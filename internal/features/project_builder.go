package features

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/davinakmalyasha/agents-proxy/internal/middleware"
)

// BuildProjectRouter mounts one project's API under /v1/p/<slug>/...
//
// Unlike BuildVerticalRouter, the feature set is scoped to the project:
// two projects may both own a feature called "bookings" without sharing
// handlers or tables. The project's business tables live in its own schema,
// and records.WithTenantTx sets search_path from clients.db_schema.
func BuildProjectRouter(project, schema string, deps BuilderDeps, vertical string) http.Handler {
	return buildProjectRouter(project, schema, deps, vertical, true)
}

// BuildProjectRouterForTest mounts a project's features without the
// RequireProject identity gate. Feature packages test two isolated tenants
// against cloned schemas, so they need the tenant transaction and feature
// gates but cannot satisfy a fixed project slug. Production always calls
// BuildProjectRouter; the project-slug check has its own API test.
func BuildProjectRouterForTest(project string, deps BuilderDeps, vertical string) http.Handler {
	return buildProjectRouter(project, ProjectSchema(project), deps, vertical, false)
}

func buildProjectRouter(project, schema string, deps BuilderDeps, vertical string, requireProject bool) http.Handler {
	if !IsAllowedVertical(vertical) {
		panic("features.BuildProjectRouter: unknown vertical " + vertical)
	}
	recDeps := records.Deps{Pool: deps.Pool, Cache: deps.Cache}
	r := chi.NewRouter()
	if deps.Auth != nil {
		r.Use(records.WithIdentity(records.IdentityDeps{Pool: deps.Pool, Cache: deps.Cache, Auth: deps.Auth}))
	} else {
		r.Use(records.WithClientAuth(recDeps))
	}
	r.Use(RequireTenantHealthy)
	if requireProject {
		r.Use(RequireProject(project, schema))
	}
	if deps.Redis != nil && deps.RateLimitPerMinute > 0 {
		r.Use(middleware.RateLimitClient(deps.Redis, deps.RateLimitPerMinute, "project:"+project))
	}

	rec := records.New(recDeps)
	r.Mount("/records", rec.Routes(vertical))

	mounted := 0
	for _, f := range ProjectFeatures(project) {
		if f.Name == "records" {
			continue
		}
		if !projectFeatureAllowed(f, vertical) {
			continue
		}
		gate := RequireProjectFeature(project, f.Name, f.PublicRead)
		if f.Routes != nil {
			f := f
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
		f := f
		r.Route("/"+f.Name, func(rr chi.Router) {
			rr.Use(gate)
			rr.Mount("/", adapter.Routes())
		})
		mounted++
	}
	slog.Debug("project mounted", "project", project, "vertical", vertical, "features", mounted)
	return r
}

func projectFeatureAllowed(f Feature, vertical string) bool {
	if len(f.VerticalSlugs) == 0 {
		return true
	}
	for _, v := range f.VerticalSlugs {
		if v == vertical {
			return true
		}
	}
	return false
}
