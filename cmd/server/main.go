// DONE: PRD 7 (server hardening + graceful shutdown) / STANDARDS A, B, D, H (slog, recovery, Sentry env-gated init, version ldflags)
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"syscall"
	"time"

	"github.com/davinakmalyasha/agents-proxy/AgentsTools"
	_ "github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/tools"
	"github.com/davinakmalyasha/agents-proxy/internal/adkrunner"
	"github.com/davinakmalyasha/agents-proxy/internal/api"
	"github.com/davinakmalyasha/agents-proxy/internal/authapi"
	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/config"
	"github.com/davinakmalyasha/agents-proxy/internal/db"
	// Phase 3 shared features: each registers itself in init().
	_ "github.com/davinakmalyasha/agents-proxy/internal/features/shared/article"
	_ "github.com/davinakmalyasha/agents-proxy/internal/features/shared/booking"
	_ "github.com/davinakmalyasha/agents-proxy/internal/features/shared/menu"
	_ "github.com/davinakmalyasha/agents-proxy/internal/features/shared/promo"
	_ "github.com/davinakmalyasha/agents-proxy/internal/features/shared/review"
	// Phase 4/6 sports-vertical custom features.
	_ "github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/availability"
	"github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/bookings"
	_ "github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/courtblocks"
	_ "github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/customers"
	_ "github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/expenses"
	_ "github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/facilities"
	_ "github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/inventory"
	_ "github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/kantin"
	_ "github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/members"
	_ "github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/payments"
	_ "github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/pricing"
	_ "github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/series"
	telegram_feature "github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/telegram"
	_ "github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/users"
	"github.com/davinakmalyasha/agents-proxy/internal/llm"
	"github.com/davinakmalyasha/agents-proxy/internal/notify"
	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
	"github.com/davinakmalyasha/agents-proxy/internal/reminder"
	"github.com/davinakmalyasha/agents-proxy/internal/sentry"
	"github.com/davinakmalyasha/agents-proxy/internal/telegram"
	"github.com/davinakmalyasha/agents-proxy/internal/tgvault"
)

func main() {
	// Sentry FIRST: env-gated no-op when SENTRY_DSN is empty (PRD Â§H).
	// Re-read env directly so Init runs even before config.Load.
	sentry.Init(sentry.Options{
		DSN:                os.Getenv("SENTRY_DSN"),
		Environment:        os.Getenv("SENTRY_ENV"),
		Release:            firstNonEmpty(os.Getenv("SENTRY_RELEASE"), Version),
		TracesSampleRate:   parseFloatOr(os.Getenv("TRACES_SAMPLE_RATE"), 0.1),
		ProfilesSampleRate: parseFloatOr(os.Getenv("PROFILES_SAMPLE_RATE"), 0.1),
		ServerName:         "agents-proxy",
	})
	defer sentry.Flush(2 * time.Second)

	// slog JSON handler with Authorization redaction. Wraps a Sentry-
	// forwarding handler so Error records go to Sentry automatically.
	redact := func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == "Authorization" {
			return slog.String("Authorization", "[REDACTED]")
		}
		return a
	}
	inner := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo, ReplaceAttr: redact})
	slog.SetDefault(slog.New(sentry.NewSlogHandler(inner)))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load", "err", err)
		os.Exit(1)
	}
	slog.Info("config loaded", "summary", cfg.String())

	bootCtx, bootCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer bootCancel()

	// Runtime pool connects with the (potentially non-superuser)
	// app_runtime role. In production this DSN points at the
	// Supabase Supavisor 6543 pooler with simple protocol; the
	// admin DSN points at 5432 direct for DDL.
	pool, err := db.NewPoolWithOptions(bootCtx, cfg.DatabaseURL, db.NewPoolOptions{
		MinConns:          cfg.DBPoolMin,
		MaxConns:          cfg.DBPoolMax,
		UseSimpleProtocol: cfg.DBSimpleProtocol,
	})
	if err != nil {
		slog.Error("db connect", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	slog.Info("db connected")

	// Migrations use a separate admin connection (default = same DSN).
	// Production (Supabase) uses postgres user for DDL; the runtime
	// pooler user is non-superuser so RLS enforces.
	adminDSN := cfg.DatabaseAdminURL
	if adminDSN == "" {
		adminDSN = cfg.DatabaseURL
	}
	adminPool, err := db.NewPool(bootCtx, adminDSN)
	if err != nil {
		slog.Error("db admin connect", "err", err)
		os.Exit(1)
	}
	defer adminPool.Close()
	if err := db.Migrate(bootCtx, adminPool); err != nil {
		slog.Error("migrate", "err", err)
		os.Exit(1)
	}
	slog.Info("migrations applied")

	rdb, err := redisx.NewClient(bootCtx, cfg.RedisURL)
	if err != nil {
		slog.Error("redis connect", "err", err)
		os.Exit(1)
	}
	defer rdb.Close()
	slog.Info("redis connected")
	agentstools.ReportRDB = rdb // download tokens for chat-generated .xlsx reports
	agentstools.CapRDB = rdb    // per-tenant daily cost cap (unconditional â€” must not depend on Telegram)

	cli := &cache.Client{DB: pool, RDB: rdb}
	llmCli := llm.New(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel, cfg.MaxTokensPerReq)

	// Per-tenant agent configuration, from internal/agentconfig/projects/.
	// Loaded and validated against the tool registry here, once, and fatal
	// on failure.
	//
	// Fatal is the point. Every mistake validation catches -- a tool named in
	// tools.json that nothing implements, a role a project granted itself, a
	// skill whose file is missing, a project claiming a write is a read --
	// produces an agent that looks configured and quietly cannot do its job.
	// The owner finds out at 3am as "I couldn't reach the system"; the
	// deploy finds out in a second.
	projects, err := agentstools.LoadProjects(agentstools.NewToolSet(pool))
	if err != nil {
		slog.Error("agent project config is invalid", "err", err)
		return
	}
	names := make([]string, 0, len(projects))
	for slug, p := range projects {
		names = append(names, slug+" ("+p.Display+")")
	}
	sort.Strings(names)
	slog.Info("agent projects loaded", "count", len(projects), "projects", names)

	// ADK agent loop (cutover from internal/agent's hand-rolled loop):
	// conversation sessions live on Postgres (migration 041), one
	// llmagent per (tenant, role) built per call from the tenant's four
	// folders. Missing session tables fail boot, same fail-loud
	// philosophy as LoadProjects above.
	if err := adkrunner.VerifySessionTables(bootCtx, pool); err != nil {
		slog.Error("adk session tables", "err", err)
		return
	}
	sessSvc, err := adkrunner.NewSessionService(pool)
	if err != nil {
		slog.Error("adk session service", "err", err)
		return
	}
	adkRunner, err := adkrunner.New(adkrunner.Config{
		Pool:      pool,
		Projects:  projects,
		BaseURL:   cfg.LLMBaseURL,
		APIKey:    cfg.LLMAPIKey,
		Model:     cfg.LLMModel,
		MaxTokens: cfg.MaxTokensPerReq,
		Sessions:  sessSvc,
	})
	if err != nil {
		slog.Error("adk runner", "err", err)
		return
	}
	slog.Info("adk runner ready", "model", cfg.LLMModel)

	// Notification queue (Phase 4 sports vertical). Durable in
	// notify_jobs (migration 029) rather than an in-process channel, so
	// a job survives a serverless invocation being destroyed
	// mid-request.
	//
	// Enqueue goes through adminPool: the call happens after the
	// booking transaction is built but before it commits, so there is no
	// app.tenant_id GUC set, and notify_jobs is FORCE RLS.
	notifyQ := notify.New(adminPool)
	if cfg.BackgroundTickers {
		notifyQ.Start(notify.WaMeSender{})
	} else {
		slog.Info("notify drain disabled; Supabase webhook + pg_cron own delivery")
	}
	defer notifyQ.Stop()
	bookings.NotifyFunc = func(tenantID, to, text string) {
		// Never let a queue hiccup fail the booking.
		if err := notifyQ.Enqueue(context.Background(), tenantID, to, text); err != nil {
			slog.Warn("notify enqueue", "tenant", tenantID, "to", to, "err", err)
		}
	}

	// ---- Multi-bot Telegram (T3): bots live in telegram_bots, tokens
	// encrypted with TELEGRAM_VAULT_KEY. Webhooks in prod, per-bot
	// polling fallback for dev (TELEGRAM_POLL_DEV=1).
	tgVaultKey, vaultErr := tgvault.MasterKeyFromEnv()
	if vaultErr != nil {
		slog.Warn("telegram bots disabled: " + vaultErr.Error() + " (see .env.example TELEGRAM_VAULT_KEY)")
	}
	var tgMgr *telegram.Manager
	var reminderStop func()
	if vaultErr == nil {
		telegram_feature.VaultKey = tgVaultKey
		telegram_feature.PublicURL = cfg.PublicURL
		// Cutover complete for Telegram: the shared ADK runner (role is
		// per-bot from the bots table, tools filtered per role).
		tgMgr = telegram.NewManager(adminPool, adkRunner, tgVaultKey, cfg.PublicURL, cfg.TelegramPollDev, cfg.TelegramConcurrency)
		tgMgr.Sync(context.Background())
		slog.Info("telegram manager started", "bots", tgMgr.Count(),
			"webhooks", cfg.PublicURL != "", "pollDev", cfg.TelegramPollDev)

		// Reconcile every minute (new/changed/disabled bots).
		tgCtx, tgCancel := context.WithCancel(context.Background())
		go func() {
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			for {
				select {
				case <-tgCtx.Done():
					return
				case <-t.C:
					tgMgr.Sync(context.Background())
				}
			}
		}()
		reminderStop = func() {
			tgCancel()
		}

		// Reminder scheduler (admin pool so it can see all tenants'
		// due reminders; per-tenant UPDATEs are RLS-scoped; delivery
		// goes through each reminder's owning bot).
		//
		// BACKGROUND_TICKERS=off on serverless: the function is destroyed
		// between requests so the 60s ticker would never tick, and
		// Supabase pg_cron + the deliver-reminders Edge Function owns
		// delivery instead. Both drivers claim rows (migration 030) so
		// enabling both is safe, but only one should be active.
		remSched := reminder.NewWithKey(adminPool, nil, tgVaultKey)
		if cfg.BackgroundTickers {
			remSched.Start()
		} else {
			slog.Info("reminder ticker disabled; delivery owned by Supabase pg_cron")
		}
		oldStop := reminderStop
		reminderStop = func() { oldStop(); remSched.Stop(); tgMgr.Close() }
	}
	defer func() {
		if reminderStop != nil {
			reminderStop()
		}
	}()

	router := api.NewRouter(api.Deps{
		Cfg:       cfg,
		Pool:      pool,
		Redis:     rdb,
		Cache:     cli,
		LLM:       llmCli,
		ADK:       adkRunner,
		Projects:  projects,
		Auth:      &authapi.Auth{Pool: pool, Cache: cli},
		Version:   Version,
		TGBots:    tgMgr,
		AdminPool: adminPool,
		AdminKey:  cfg.AdminAPIKey,
	})

	// Background disk reclaim for chat-generated .xlsx (not on the
	// download request path).
	reportStop := make(chan struct{})
	reportsSweeper := &api.Reports{RDB: rdb}
	go reportsSweeper.StartSweeper(reportStop)

	// Retention janitor: expired sessions + old sent/dead reminders
	// (migration 027). Hourly, best-effort, on the admin pool (sees all
	// tenants; the deletes are by status/timestamp, not tenant-scoped).
	janitorStop := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		runJanitor := func() {
			cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer ccancel()
			if _, err := adminPool.Exec(cctx, `DELETE FROM sports_sessions WHERE expires_at < now() - interval '7 days'`); err != nil {
				slog.Warn("janitor sessions", "err", err)
			}
			if _, err := adminPool.Exec(cctx, `DELETE FROM agent_reminders WHERE status='sent' AND created_at < now() - interval '90 days'`); err != nil {
				slog.Warn("janitor sent reminders", "err", err)
			}
			if _, err := adminPool.Exec(cctx, `DELETE FROM agent_reminders WHERE status='dead' AND created_at < now() - interval '30 days'`); err != nil {
				slog.Warn("janitor dead reminders", "err", err)
			}
		}
		runJanitor()
		for {
			select {
			case <-janitorStop:
				return
			case <-t.C:
				runJanitor()
			}
		}
	}()

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// WriteTimeout bounds slow responses/hijacked streams; agent
		// LLM calls (â‰¤60s in llm.Client) exceed it, but those paths
		// don't block on client writes â€” they respond at the end. Set
		// generous 90s so long provider replies still flush.
		WriteTimeout: 90 * time.Second,
		IdleTimeout:  120 * time.Second,
		// TLSConfig left nil for plain HTTP local; PQC default in 1.27 only applies to TLS.
	}

	// Graceful shutdown.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		slog.Info("listening", "addr", srv.Addr, "env", cfg.Env, "model", cfg.LLMModel)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("listen", "err", err)
			os.Exit(1)
		}
	}()

	sig := <-stop
	slog.Info("shutdown signal", "sig", sig.String())
	close(reportStop)
	close(janitorStop)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown", "err", err)
	}
	// Give Sentry a final chance to flush queued events.
	sentry.Flush(2 * time.Second)
	slog.Info("bye")
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func parseFloatOr(s string, def float64) float64 {
	if s == "" {
		return def
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return def
	}
	return f
}
