// DONE: PRD 3 router, 6 API contracts / STANDARDS A, B, F
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/adkrunner"
	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
	"github.com/davinakmalyasha/agents-proxy/internal/auth"
	"github.com/davinakmalyasha/agents-proxy/internal/authapi"
	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/config"
	"github.com/davinakmalyasha/agents-proxy/internal/features"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/davinakmalyasha/agents-proxy/internal/llm"
	"github.com/davinakmalyasha/agents-proxy/internal/mcp"
	"github.com/davinakmalyasha/agents-proxy/internal/middleware"
	"github.com/davinakmalyasha/agents-proxy/internal/telegram"
)

type Deps struct {
	Cfg   *config.Config
	Pool  *pgxpool.Pool
	Redis *redis.Client
	Cache *cache.Client
	LLM   *llm.Client
	// ADK is the cutover loop. Non-nil in production; tests that only
	// exercise pre-loop rejections may leave it nil, in which case the
	// not-yet-migrated call sites fall back to the old loop.
	ADK     *adkrunner.Runner
	Auth    *authapi.Auth
	Version string
	TGBots  *telegram.Manager // nil when vault not configured
	// Admin (super-operator) wiring for the MCP admin tools + health.
	AdminPool *pgxpool.Pool // nil when the app pool doubles for it
	AdminKey  string        // ADMIN_API_KEY
	// Projects is the per-tenant agent configuration from
	// internal/agentconfig, validated against the tool registry at boot.
	// nil is supported: agents then run on the platform base prompt alone.
	Projects map[string]*agentconfig.Project
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
	r.Use(middleware.CORS(d.Cfg.AllowOrigins))

	r.Get("/healthz", (&Healthz{DB: d.Pool, RDB: d.Redis, Build: d.Version}).ServeHTTP)

	// Telegram webhooks: secret-gated per bot, intentionally outside the
	// rate-limited /v1 group (Telegram retries bursts; the webhook
	// secret IS the auth). Nil-safe when the vault isn't configured.
	if d.TGBots != nil {
		r.Post("/telegram/hook/{id}", d.TGBots.ServeHook)
	}

	r.Route("/v1", func(r chi.Router) {
		// Global per-client/IP rate limit (default 180). Chat gets stricter sub-limit below.
		r.Use(middleware.RateLimit(d.Redis, d.Cfg.RateLimitPerMinute))

		chatH := NewChat(d.Pool, d.Cache, d.LLM, d.Redis, d.Cfg.RateLimitPerMinute)
		r.Post("/chat", chatH.ServeHTTP)

		// Agent chat — single shared LLM_API_KEY, 1 bot shared, 3 doors (client/pegawai/owner)
		// Client: public landing widget (FAQ/availability only), no money
		// Staff/Owner: via X-Session-Token, role-aware toolsets + per-role caps
		// Stricter per-IP limit for anon client chat to deter waste (20/min/IP)
		agentH := &AgentChat{
			Pool:     d.Pool,
			RDB:      d.Redis,
			Auth:     d.Auth,
			Projects: d.Projects,
			ADK:      d.ADK,
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

		// WS2: chat-generated .xlsx downloads — token IS the auth (tenant-scoped, 30-min TTL)
		reportsH := &Reports{RDB: d.Redis}
		r.Get("/reports/{token}", reportsH.Download)

		// MCP — Go-only, thin adapter so new client "just setup mcp/tool".
		// Exposes role ToolSets + admin_* cross-tenant tools (Bearer key).
		mcpSrv := mcp.New(d.Pool, d.Cache, d.Auth, d.AdminPool, d.AdminKey)
		r.Handle("/mcp", mcpSrv)
		r.Handle("/mcp/*", mcpSrv)

		// Staff login (Phase 7): email+password → session token.
		if d.Auth != nil {
			r.Mount("/auth", d.Auth.Routes())
		}

		// Per-vertical feature platform. One builder call per
		// vertical; the builder mounts the records feature plus
		// every registered shared feature, each gated.
		bDeps := features.BuilderDeps{
			Pool:               d.Pool,
			Cache:              d.Cache,
			Auth:               d.Auth,
			Redis:              d.Redis,
			RateLimitPerMinute: d.Cfg.RateLimitPerMinute,
		}
		for _, v := range features.AllowedVerticals {
			v := v
			r.Mount("/"+v, features.BuildVerticalRouter(bDeps, v))
		}

		// The admin surface WRITES to clients (POST /clients), and
		// 005_rbac.sql grants app_runtime only SELECT on that table. Running
		// these handlers on the app pool therefore made every write fail with
		// SQLSTATE 42501, which admin.Create reported as a bare 500
		// "internal" — leaving no supported way to provision a tenant at all.
		// Use the admin pool, falling back to the app pool when the caller
		// doubles them (see Deps.AdminPool).
		adminPool := d.AdminPool
		if adminPool == nil {
			adminPool = d.Pool
		}

		r.Route("/admin", func(r chi.Router) {
			r.Use(auth.RequireAdmin(d.Cfg.AdminAPIKey))
			adm := &Admin{Pool: adminPool, Cache: d.Cache}
			r.Post("/clients", adm.Create)
			r.Get("/clients", adm.List)
			r.Patch("/clients/{id}", records.WithValidID(adm.Patch))
			r.Get("/usage", adm.Usage)
		})
	})

	return r
}
