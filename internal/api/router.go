// DONE: PRD 3 router, 6 API contracts / STANDARDS A, B, F
package api

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/adkrunner"
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
	AdminKey  string        // ADMIN_API_KEY
	// Origins is the DB-backed CORS registry (migration 058). Nil keeps
	// the static env list only (tests, local dev); production wires it
	// at boot and exits when it cannot load (fail closed).
	Origins *middleware.DBResolver
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

		// AdminPool for the admin surface below and the report-jobs
		// worker (cross-tenant claim). Fallback when the caller doubles
		// them (see Deps.AdminPool).
		adminPool := d.AdminPool
		if adminPool == nil {
			adminPool = d.Pool
		}

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

		// Per-vertical feature platform. Only verticals with live tenants
		// mount (plus any vertical with registered custom features);
		// the rest serve nothing today. Zero live tenants — a fresh
		// database — mounts everything, exactly as before.
		bDeps := features.BuilderDeps{
			Pool:               d.Pool,
			Cache:              d.Cache,
			Auth:               d.Auth,
			Redis:              d.Redis,
			RateLimitPerMinute: d.Cfg.RateLimitPerMinute,
		}
		for _, v := range features.VerticalsToMount(features.LiveVerticals(context.Background(), d.Pool)) {
			v := v
			r.Mount("/"+v, features.BuildVerticalRouter(bDeps, v))
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
			adm := &Admin{Pool: adminPool, Cache: d.Cache}
			r.Post("/clients", adm.Create)
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

	return r
}
