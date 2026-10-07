// DONE: PRD 7 (server hardening + graceful shutdown) / STANDARDS A, B, D, H (slog, recovery, Sentry env-gated init, version ldflags)
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/adkrunner"
	"github.com/davinakmalyasha/agents-proxy/internal/api"
	"github.com/davinakmalyasha/agents-proxy/internal/authapi"
	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/config"
	"github.com/davinakmalyasha/agents-proxy/internal/db"
	_ "github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/tools"
	// Phase 3 shared features: each registers itself in init().
	_ "github.com/davinakmalyasha/agents-proxy/internal/features/shared/article"
	_ "github.com/davinakmalyasha/agents-proxy/internal/features/shared/booking"
	_ "github.com/davinakmalyasha/agents-proxy/internal/features/shared/menu"
	_ "github.com/davinakmalyasha/agents-proxy/internal/features/shared/promo"
	_ "github.com/davinakmalyasha/agents-proxy/internal/features/shared/review"
	// Phase 4/6 sports-vertical custom features.
	"github.com/davinakmalyasha/agents-proxy/internal/llm"
	"github.com/davinakmalyasha/agents-proxy/internal/notify"
	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
	"github.com/davinakmalyasha/agents-proxy/internal/reminder"
	"github.com/davinakmalyasha/agents-proxy/internal/sentry"
	"github.com/davinakmalyasha/agents-proxy/internal/tenanthealth"

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
	_ "github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/backend/users"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load", "err", err)
		os.Exit(1)
	}

	// Sentry after config: a single source for DSN/env/release/rate.
	// config.Load cannot fail on a missing DSN (empty disables), so the
	// old direct os.Getenv reads were pure duplication.
	release := cfg.SentryRelease
	if release == "" {
		release = Version
	}
	sentry.Init(sentry.Options{
		DSN:              cfg.SentryDSN,
		Environment:      cfg.SentryEnv,
		Release:          release,
		TracesSampleRate: cfg.TracesSampleRate,
		ServerName:       "agents-proxy",
	})
	defer sentry.Flush(2 * time.Second)

	// slog JSON handler with secret redaction. Uses the same sensitive-key
	// list as the Sentry scrubber so the two can never drift apart.
	redact := func(_ []string, a slog.Attr) slog.Attr {
		if sentry.IsSensitive(a.Key) {
			return slog.String(a.Key, "[REDACTED]")
		}
		return a
	}
	inner := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo, ReplaceAttr: redact})
	slog.SetDefault(slog.New(sentry.NewSlogHandler(inner)))

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
		ApplicationName:   "agentsproxy/" + cfg.Env,
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
	// Sized deliberately small: this pool talks to DIRECT 5432 in session
	// mode, where Supabase caps sessions (pool_size 15). The old default
	// (Min 2 / Max 20) parked idle sessions per instance and cold-start
	// storms exhausted the cap with EMAXCONNSESSION. Min 0 parks nothing;
	// Max 3 bounds migrate + admin endpoints + one worker. Idle sessions
	// close after a minute so cold-start instances stop counting against
	// the cap almost immediately.
	adminPool, err := db.NewPoolWithOptions(bootCtx, adminDSN, db.NewPoolOptions{
		MinConns:        cfg.DBAdminPoolMin,
		MaxConns:        cfg.DBAdminPoolMax,
		MaxConnIdleTime: time.Minute,
		ApplicationName: "agentsproxy/" + cfg.Env + "-admin",
	})
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
	agentstools.CapRDB = rdb    // per-tenant daily cost cap
	agentstools.ReportMaxBytes = cfg.ReportMaxBytes
	agentstools.ConfigureSupabaseReportStore(cfg.SupabaseURL, cfg.SupabaseStorageKey, cfg.ReportBucket)

	cli := &cache.Client{DB: pool, RDB: rdb}
	llmCli := llm.New(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel, cfg.MaxTokensPerReq)

	// Per-tenant agent configuration, collected not thrown: each tenant
	// validates independently. A bad tenant is quarantined (fenced with
	// an honest 503) while healthy tenants serve. Only zero healthy
	// tenants -- or a load failure -- is fatal for everyone.
	//
	// Fatal is still the point for shared mistakes: a config that cannot
	// even be listed means no tenant can be evaluated, and the deploy
	// must fail loudly rather than serve an empty world.
	projects, badTenants, err := agentstools.LoadProjectsCollect(agentstools.AllTools(pool))
	if err != nil {
		slog.Error("agent project config unloadable", "err", err)
		os.Exit(1)
	}
	if len(projects) == 0 {
		slog.Error("agent project config: zero healthy tenants")
		os.Exit(1)
	}
	for slug, reason := range badTenants {
		tenanthealth.Quarantine(slug, "boot: "+reason)
		slog.Warn("tenant quarantined at boot", "tenant", slug, "reason", reason)
	}
	// Quarantine is durable (tenant_quarantine) so the Supabase driver
	// honors it too. Reconcile both directions on the admin pool:
	// boot-fenced rows are written, and rows fenced by a previous
	// instance (or by hand) are loaded into this process's registry.
	reconcileQuarantine(bootCtx, adminPool, badTenants)
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
		os.Exit(1)
	}
	sessSvc, err := adkrunner.NewSessionService(pool)
	if err != nil {
		slog.Error("adk session service", "err", err)
		os.Exit(1)
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
		os.Exit(1)
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
	bookings.NotifyFunc = func(ctx context.Context, tenantID, to, text string) {
		// Detached: the booking already committed, so a client disconnect
		// must not drop the notification. Bounded so a hung DB cannot
		// hold this goroutine forever.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		// Never let a queue hiccup fail the booking.
		if err := notifyQ.Enqueue(dctx, tenantID, to, text); err != nil {
			slog.Warn("notify enqueue", "tenant", tenantID, "to", to, "err", err)
		}
	}

	// Reminder scheduler (admin pool so it can see all tenants' due
	// reminders; per-row completion UPDATEs are RLS-scoped).
	//
	// Telegram delivery was deleted. There is currently no PWA delivery
	// transport, so the scheduler preserves due rows until that lands
	// instead of dead-lettering them. BACKGROUND_TICKERS=false on
	// serverless: the function is destroyed between requests so the 60s
	// ticker would never tick.
	remSched := reminder.New(adminPool, nil)
	if cfg.BackgroundTickers {
		remSched.Start()
	} else {
		slog.Info("reminder ticker disabled on serverless")
	}
	defer remSched.Stop()

	router := api.NewRouter(api.Deps{
		Cfg:       cfg,
		Pool:      pool,
		Redis:     rdb,
		Cache:     cli,
		LLM:       llmCli,
		ADK:       adkRunner,
		Auth:      &authapi.Auth{Pool: pool},
		Version:   Version,
		AdminPool: adminPool,
		AdminKey:  cfg.AdminAPIKey,
	})

	// Background reclaim for chat-generated .xlsx (not on the
	// download request path). Manifest-driven purge handles durable
	// storage; the legacy local walk covers pre-manifest dev files.
	reportStop := make(chan struct{})
	reportsSweeper := &api.Reports{Pool: pool}
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
		// WriteTimeout covers the slowest response path: one agent turn
		// (adkrunner.TurnTimeout) plus slack to flush the reply. The
		// turn has its own deadline and maps overruns to 504, so this
		// is strictly a backstop, derived from the same constant.
		WriteTimeout: adkrunner.TurnTimeout + 10*time.Second,
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

// reconcileQuarantine syncs the in-memory registry with the durable
// tenant_quarantine table (both directions, admin pool):
//   - rows fenced at this boot are written, so the Supabase driver honors
//     them and a restart does not silently unfence;
//   - rows already present (previous instance, or fenced by hand) are
//     loaded into this process's registry.
// A boot failure here is fatal: silent divergence between the registry
// and the table would fence tenants for one driver but not the other.
func reconcileQuarantine(ctx context.Context, adminPool *pgxpool.Pool, badTenants map[string]string) {
	for slug, reason := range badTenants {
		if _, err := adminPool.Exec(ctx,
			`INSERT INTO tenant_quarantine (tenant_id, reason) VALUES ($1, $2)
			 ON CONFLICT (tenant_id) DO UPDATE SET reason = EXCLUDED.reason, quarantined_at = now()`,
			slug, "boot: "+reason); err != nil {
			slog.Error("quarantine persist failed", "tenant", slug, "err", err)
			os.Exit(1)
		}
	}
	rows, err := adminPool.Query(ctx, `SELECT tenant_id, reason FROM tenant_quarantine`)
	if err != nil {
		slog.Error("quarantine load failed", "err", err)
		os.Exit(1)
	}
	defer rows.Close()
	for rows.Next() {
		var slug, reason string
		if err := rows.Scan(&slug, &reason); err != nil {
			slog.Error("quarantine scan failed", "err", err)
			os.Exit(1)
		}
		if _, have := badTenants[slug]; !have {
			slog.Warn("tenant quarantined by previous state", "tenant", slug, "reason", reason)
		}
		tenanthealth.Quarantine(slug, reason)
	}
	if err := rows.Err(); err != nil {
		slog.Error("quarantine rows failed", "err", err)
		os.Exit(1)
	}
}
