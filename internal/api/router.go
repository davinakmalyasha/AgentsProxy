// DONE: PRD 3 router, 6 API contracts / STANDARDS A, B, F
package api

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/adkrunner"
	"github.com/davinakmalyasha/agents-proxy/internal/agency"
	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
	"github.com/davinakmalyasha/agents-proxy/internal/auth"
	"github.com/davinakmalyasha/agents-proxy/internal/authapi"
	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/config"
	"github.com/davinakmalyasha/agents-proxy/internal/features"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/davinakmalyasha/agents-proxy/internal/llm"
	"github.com/davinakmalyasha/agents-proxy/internal/middleware"
)

type Deps struct {
	Cfg   *config.Config
	Pool  *pgxpool.Pool
	Redis *redis.Client
	Cache *cache.Client
	LLM   *llm.Client
	// ADK is the agent loop. Boot is fatal without it.
	ADK     *adkrunner.Runner
	Auth    *authapi.Auth
	Version string
	// Admin (super-operator) wiring for the REST /admin surface.
	AdminPool *pgxpool.Pool // nil when the app pool doubles for it
	// Origins is the DB-backed CORS registry (migration 058). Nil keeps
	// the static env list only (tests, local dev); production wires it
	// at boot and exits when it cannot load (fail closed).
	Origins *middleware.DBResolver
	// Projects is the provisioning catalog. Nil uses the embedded project
	// folders; tests inject a fixture catalog.
	Projects ProjectCatalog
}

func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recovery())
	// Pure additive hardening headers on every response (incl. healthz).
	r.Use(middleware.SecurityHeaders())
	r.Use(middleware.RequestID())
	r.Use(middleware.Logging())
	// Normalize trailing slashes BEFORE routing (internal rewrite, all
	// methods): chi patterns are exact and features mix "/" vs "/name"
	// styles, which 404s half the matrix otherwise.
	r.Use(middleware.StripTrailingSlash)
	// CORS: static env list ∪ DB registry (nil resolver = static only).
	r.Use(middleware.CORSResolving(d.Cfg.AllowOrigins, d.Origins))
	// Compress JSON responses over 1KB (list endpoints); already-
	// compressed bodies (.xlsx) and small payloads pass through.
	r.Use(middleware.Gzip)

	r.Get("/healthz", (&Healthz{DB: d.Pool, RDB: d.Redis, Build: d.Version}).ServeHTTP)

	// AdminPool for the admin surface below and the report-jobs
	// worker (cross-tenant claim). Fallback when the caller doubles
	// them (see Deps.AdminPool). The agency plane below reuses it:
	// agency tables grant app_runtime, and admin ops need no tenant GUC.
	adminPool := d.AdminPool
	if adminPool == nil {
		adminPool = d.Pool
	}

	r.Route("/v1", func(r chi.Router) {
		// Global per-client/IP rate limit (default 180). Chat gets stricter sub-limit below.
		r.Use(middleware.RateLimit(d.Redis, d.Cfg.RateLimitPerMinute))

		chatH := NewChat(d.Pool, d.Cache, d.LLM, d.Redis, d.Cfg.RateLimitPerMinute)
		r.Post("/chat", chatH.ServeHTTP)

		// Agent chat — single shared LLM_API_KEY, 3 doors (client/pegawai/owner)
		// Client: public landing widget (FAQ/availability only), no money
		// Staff/Owner: via X-Session-Token, role-aware toolsets + per-role caps
		// Stricter per-IP limit for anon client chat to deter waste (20/min/IP)
		agentH := &AgentChat{
			Pool: d.Pool,
			RDB:  d.Redis,
			Auth: d.Auth,
			ADK:  d.ADK,
			sem:  make(chan struct{}, maxConcurrentAgentTurns),
		}
		r.Route("/agent", func(r chi.Router) {
			// Isolated bucket: chat quota must not be eaten by landing polls
			// (they share the same client IP but live in the "global" scope).
			r.Use(middleware.RateLimitScope(d.Redis, d.Cfg.AgentRateLimitPerMinute, "agent"))
			r.Post("/client-chat", agentH.ClientChat)
			r.Post("/chat", agentH.Chat)
			// Client-only memory wipe (landing "Bersihkan riwayat"); staff/owner keep theirs
			r.Delete("/history", agentH.ClearHistory)
		})

		// AdminPool for the /v1/admin surface already computed above
		// (falls back to the app pool when the caller doubles them).

		// WS2: chat-generated .xlsx downloads — token IS the auth (tenant-scoped, 30-min TTL)
		reportsH := &Reports{Pool: d.Pool}
		// Deferred report builds (migration 064): the agent tool returns
		// a job link; pg_cron claims one job per tick via the internal
		// endpoint. The token header keeps the build surface closed.
		jobsH := &ReportJobs{Pool: d.Pool, AdminPool: adminPool, Token: d.Cfg.ReportWorkerToken}
		r.Get("/reports/jobs/{id}", jobsH.Get)
		r.Post("/internal/report-jobs/build", jobsH.Build)
		r.Get("/reports/{token}", reportsH.Download)

		// Staff login (Phase 7): email+password → session token.
		if d.Auth != nil {
			r.Mount("/auth", d.Auth.Routes())
		}

		// PWA push subscriptions (Phase 2): staff-session authed,
		// tenant-scoped by the session. The app pool + GUC scoping
		// match every other tenant write. Skipped without an Auth
		// backend (same guard as /auth above).
		if d.Auth != nil {
			pushH := &Push{Pool: d.Pool, Auth: d.Auth}
			r.Post("/push/subscriptions", pushH.Subscribe)
			r.Delete("/push/subscriptions", pushH.Unsubscribe)
		}

		// Per-vertical feature platform for platform tenants only. A project
		// tenant is mounted exclusively under /v1/p/<slug>; it never creates
		// a /v1/{vertical} alias. No live platform vertical means this
		// surface is empty, which is the intended state for a project-only
		// deployment.
		bDeps := features.BuilderDeps{
			Pool:               d.Pool,
			Cache:              d.Cache,
			Auth:               d.Auth,
			Redis:              d.Redis,
			RateLimitPerMinute: d.Cfg.RateLimitPerMinute,
		}
		// Mount the platform surface statically. Route availability must not
		// depend on tenant rows: a new platform tenant of a new vertical must
		// be reachable immediately, and a project tenant must never gain the
		// platform surface merely because another tenant shares its vertical.
		// RequirePlatformTenant in the builder is the access boundary.
		for _, v := range features.AllowedVerticals {
			v := v
			r.Mount("/"+v, features.BuildVerticalRouter(bDeps, v))
		}

		// Project-scoped APIs for every project. Each project owns its
		// handlers and its schema; /v1/p/<slug>/... cannot reach another
		// project's router or data.
		projectCfgs, badProjects := agentconfig.LoadCollect()
		badProjectSet := make(map[string]bool, len(badProjects))
		for slug, err := range badProjects {
			badProjectSet[slug] = true
			slog.Error("project config unavailable; fencing its project router", "project", slug, "err", err)
			if !features.ValidProjectSlug(slug) {
				continue
			}
			slug := slug
			r.Mount("/p/"+slug, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSONError(w, http.StatusServiceUnavailable,
					"project configuration unavailable; the deploy is not ready")
			}))
		}
		for slug, p := range projectCfgs {
			r.Mount("/p/"+slug, features.BuildProjectRouter(slug, p.Schema, bDeps, p.Vertical))
		}
		for _, project := range features.RegisteredProjects() {
			project := project
			if _, mounted := projectCfgs[project]; mounted {
				continue
			}
			if badProjectSet[project] {
				continue // already fenced with 503 above
			}
			vertical := features.ProjectVertical(project)
			if vertical == "" {
				slog.Warn("project features registered without a vertical", "project", project)
				continue
			}
			r.Mount("/p/"+project, features.BuildProjectRouter(project, features.ProjectSchema(project), bDeps, vertical))
		}

		// The admin surface WRITES to clients (POST /clients), and
		// 005_rbac.sql grants app_runtime only SELECT on that table. Running
		// these handlers on the app pool therefore made every write fail with
		// SQLSTATE 42501, which admin.Create reported as a bare 500
		// "internal" — leaving no supported way to provision a tenant at all.
		// Use the admin pool, falling back to the app pool when the caller
		// doubles them (adminPool computed above).
		r.Route("/admin", func(r chi.Router) {
			r.Use(auth.RequireAdmin(d.Cfg.AdminAPIKey))
			projects := d.Projects
			if projects == nil {
				projects = newEmbeddedProjectCatalog(adminPool)
			}
			adm := &Admin{Pool: adminPool, Cache: d.Cache, Projects: projects, Auth: d.Auth}
			r.Post("/clients", adm.Create)
			r.Post("/clients/{id}/owner", records.WithValidID(adm.SeedOwner))
			r.Get("/clients", adm.List)
			r.Patch("/clients/{id}", records.WithValidID(adm.Patch))
			r.Post("/clients/{id}/contract", records.WithValidID(adm.ContractBump))
			r.Get("/usage", adm.Usage)
			r.Get("/origins", adm.OriginsList)
			r.Post("/origins", adm.OriginsCreate)
			r.Delete("/origins", adm.OriginsDelete)
			r.Post("/origin-suffixes", adm.SuffixCreate)
			r.Delete("/origin-suffixes", adm.SuffixDelete)
			thDeps := TenantHealthDeps{ADK: d.ADK, Pool: adminPool}
			r.Get("/tenant-health", TenantHealth(thDeps))
			r.Post("/tenant-health/reload", TenantReload(thDeps))
		})
	})

	// Agency plane (DFD ops): /api/v1/* + legacy /api/agent/* alias + /mcp.
	// JWT_SECRET enables it; empty disables (503) so proxy-only deploys
	// keep booting unchanged.
	agencyDeps := agency.Deps{
		Pool:               adminPool,
		Logger:             nil, // agency falls back to slog.Default (JSON, redacting)
		Env:                d.Cfg.Env,
		JWTSecret:          d.Cfg.AgencyJWTSecret,
		JWTTTL:             d.Cfg.AgencyJWTTTL,
		SecureCookie:       strings.EqualFold(d.Cfg.Env, "production"),
		FrontendURL:        d.Cfg.AgencyFrontend,
		AgencyWhatsapp:     d.Cfg.AgencyWhatsapp,
		GoogleClientID:     d.Cfg.GoogleClientID,
		ResendKey:          d.Cfg.ResendAPIKey,
		ResendFrom:         d.Cfg.EmailFrom,
		SignupEnabled:      d.Cfg.SignupEnabled,
		SupabaseURL:        d.Cfg.SupabaseURL,
		SupabaseServiceKey: d.Cfg.SupabaseStorageKey,
		SupabaseBucket:     d.Cfg.ReportBucket,
	}
	if d.Cfg.AgencyJWTSecret == "" {
		disabled := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSONError(w, http.StatusServiceUnavailable, "agency plane disabled (JWT_SECRET unset)")
		})
		r.Mount("/api/v1", disabled)
		r.Mount("/api/agent", disabled)
		r.Mount("/mcp", disabled)
	} else {
		r.Mount("/api/v1", agency.NewRouter(agencyDeps))
		r.Mount("/api/agent", agency.NewAgentRouter(agencyDeps))
		mcpGuarded := agency.GuardAgent(adminPool, agency.NewMCPHandler(agencyDeps))
		r.Method("POST", "/mcp", mcpGuarded)
		r.Method("GET", "/mcp", mcpGuarded)
		r.Method("DELETE", "/mcp", mcpGuarded)
	}

	return r
}
