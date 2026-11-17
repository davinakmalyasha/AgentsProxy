// Package agency is the DFD agency plane: public catalog, intake, human
// auth, client portal, back-office admin, machine agent API, and MCP tools.
//
// All routes are chi (no net/http ServeMux). The router is mounted by the
// root router at /api/v1; the agent sub-router is additionally mounted at
// /api/agent, and the MCP handler at /mcp.
package agency

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davinakmalyasha/agents-proxy/internal/agency/admin"
	"github.com/davinakmalyasha/agents-proxy/internal/agency/agent"
	agencyauth "github.com/davinakmalyasha/agents-proxy/internal/agency/auth"
	authmod "github.com/davinakmalyasha/agents-proxy/internal/agency/authmod"
	"github.com/davinakmalyasha/agents-proxy/internal/agency/client"
	"github.com/davinakmalyasha/agents-proxy/internal/agency/content"
	"github.com/davinakmalyasha/agents-proxy/internal/agency/docs"
	"github.com/davinakmalyasha/agents-proxy/internal/agency/intake"
	"github.com/davinakmalyasha/agents-proxy/internal/agency/mcpserver"
	"github.com/davinakmalyasha/agents-proxy/internal/agency/misc"
	"github.com/davinakmalyasha/agents-proxy/internal/agency/notify"
	"github.com/davinakmalyasha/agents-proxy/internal/agency/ratelimit"
)

// Notifier is the agency outbound transport (log-backed unless a provider
// adapter replaces it, mirroring the DFD design).
type Notifier = notify.Service

// Deps wires the agency plane. Pool is the app pool (agency tables grant
// app_runtime); AdminKey is unused here (agency uses JWT/bearer, never the
// proxy admin key).
type Deps struct {
	Pool         *pgxpool.Pool
	Logger       *slog.Logger
	JWTSecret    string
	JWTTTL       time.Duration
	SecureCookie bool
	FrontendURL  string

	AgencyWhatsapp string

	// GoogleClientID enables POST /auth/google; empty disables it (503).
	GoogleClientID string
	// ResendKey/ResendFrom enable the superadmin OTP email. Empty keeps
	// SendEmail log-only (dev); the OTP step fails closed in production.
	ResendKey  string
	ResendFrom string

	SupabaseURL        string
	SupabaseServiceKey string
	SupabaseBucket     string
}

func (d Deps) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

// NewRouter builds the /api/v1 chi router.
func NewRouter(d Deps) chi.Router {
	r := chi.NewRouter()
	logger := d.logger()
	notifier := notify.New(logger, d.AgencyWhatsapp).WithResend(d.ResendKey, d.ResendFrom)
	docsStore := docs.NewStore(d.Pool)

	// Public catalog reads.
	content.NewHandler(content.NewStore(d.Pool), logger).RegisterRoutes(r)

	// Public intake writes (per-IP limited) + magic-link tracking reads.
	intakeH := intake.NewHandler(intake.NewStore(d.Pool), docsStore, d.Pool, logger, notifier, d.JWTSecret, d.FrontendURL)
	intakeH.RegisterRoutes(r, ratelimit.NewRateLimiter(3, time.Hour))

	// Human auth.
	authH := authmod.NewHandler(authmod.NewStore(d.Pool), d.Pool, logger, d.JWTSecret, d.JWTTTL, d.SecureCookie, d.GoogleClientID, notifier)
	r.Route("/auth", func(r chi.Router) {
		authH.RegisterRoutes(r, ratelimit.NewRateLimiter(5, 15*time.Minute))
	})

	cookieAuth := agencyauth.CookieAuth(d.JWTSecret, d.Pool, d.JWTTTL)
	superGuard := func(next http.Handler) http.Handler {
		return cookieAuth(agencyauth.RequireRole("SUPERADMIN")(next))
	}
	cookieOnly := cookieAuth // any authenticated role (dashboard re-checks inside)
	clientGuard := func(next http.Handler) http.Handler {
		return cookieAuth(agencyauth.RequireRole("CLIENT", "SUPERADMIN", "EDITOR")(next))
	}

	// Back-office admin (SUPERADMIN) + dashboard (any role).
	adminH := admin.NewHandler(admin.NewStore(d.Pool), docsStore, d.Pool, logger, d.FrontendURL)
	adminH.RegisterRoutes(r, superGuard)
	adminH.RegisterCatalogRoutes(r, superGuard, cookieOnly)

	// Client portal.
	clientH := client.NewHandler(client.NewStore(d.Pool), docsStore, d.Pool, logger, notifier, d.FrontendURL)
	r.Route("/client", func(r chi.Router) {
		clientH.RegisterRoutes(r, clientGuard, superGuard)
	})

	// Machine agent API (bearer key, scope agent).
	r.Route("/agent", func(r chi.Router) {
		r.Use(agencyauth.APIKeyAuth(d.Pool, "agent"))
		agent.NewHandler(agent.NewStore(d.Pool), d.Pool, logger, notifier).RegisterRoutes(r)
	})

	// Misc: upload, WhatsApp stubs, AI stub (SUPERADMIN).
	misc.NewHandler(logger, misc.Config{
		SupabaseURL:        d.SupabaseURL,
		SupabaseServiceKey: d.SupabaseServiceKey,
		SupabaseBucket:     d.SupabaseBucket,
	}).RegisterRoutes(r, superGuard)

	return r
}

// NewAgentRouter builds the bearer-guarded agent chi router for the legacy
// /api/agent/* alias (same handlers as /api/v1/agent/*).
func NewAgentRouter(d Deps) chi.Router {
	r := chi.NewRouter()
	logger := d.logger()
	notifier := notify.New(logger, d.AgencyWhatsapp).WithResend(d.ResendKey, d.ResendFrom)
	r.Use(agencyauth.APIKeyAuth(d.Pool, "agent"))
	agent.NewHandler(agent.NewStore(d.Pool), d.Pool, logger, notifier).RegisterRoutes(r)
	return r
}

// NewMCPHandler builds the MCP streamable-HTTP handler (bearer-guarded by
// the caller, scope agent).
func NewMCPHandler(d Deps) http.Handler {
	logger := d.logger()
	notifier := notify.New(logger, d.AgencyWhatsapp).WithResend(d.ResendKey, d.ResendFrom)
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return mcpserver.Build(mcpserver.Deps{
			Store:    agent.NewStore(d.Pool),
			Logger:   logger,
			Notifier: notifier,
		})
	}, nil)
}

// GuardAgent wraps h with the bearer-key agent-scope guard.
func GuardAgent(pool *pgxpool.Pool, h http.Handler) http.Handler {
	return agencyauth.APIKeyAuth(pool, "agent")(h)
}
