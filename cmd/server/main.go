// DONE: PRD 7 (server hardening + graceful shutdown) / STANDARDS A, B, D, H (slog, recovery, Sentry env-gated init, version ldflags)
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/adkrunner"
	agencynotify "github.com/davinakmalyasha/agents-proxy/internal/agency/notify"
	agencyworker "github.com/davinakmalyasha/agents-proxy/internal/agency/worker"
	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
	"github.com/davinakmalyasha/agents-proxy/internal/api"
	"github.com/davinakmalyasha/agents-proxy/internal/authapi"
	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/config"
	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/internal/features"
	// Phase 3 shared features: each registers itself in init().
	_ "github.com/davinakmalyasha/agents-proxy/internal/platformfeatures"
	// Phase 4/6 sports-vertical custom features.
	"github.com/davinakmalyasha/agents-proxy/internal/httpx"
	"github.com/davinakmalyasha/agents-proxy/internal/llm"
	"github.com/davinakmalyasha/agents-proxy/internal/middleware"
	"github.com/davinakmalyasha/agents-proxy/internal/notify"
	"github.com/davinakmalyasha/agents-proxy/internal/projecthooks"
	_ "github.com/davinakmalyasha/agents-proxy/internal/projectsgen"
	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
	"github.com/davinakmalyasha/agents-proxy/internal/reminder"
	"github.com/davinakmalyasha/agents-proxy/internal/requestip"
	"github.com/davinakmalyasha/agents-proxy/internal/sentry"
	"github.com/davinakmalyasha/agents-proxy/internal/slo"
	"github.com/davinakmalyasha/agents-proxy/internal/tenanthealth"
	"github.com/davinakmalyasha/agents-proxy/internal/usage"
)

//go:generate go run ../../cmd/genprojects
func main() {
	// Healthcheck probe for container orchestration (docker-compose
	// healthcheck, k8s): exits 0 when /healthz answers 200, 1
	// otherwise. Without this the compose healthcheck boots a whole
	// second server that binds :8080 and hangs until timeout.
	if len(os.Args) > 1 && os.Args[1] == "--healthcheck" {
		os.Exit(healthcheck())
	}
	bootStart := time.Now()
	phase := func(name string, start time.Time) {
		slog.Info("boot phase", "phase", name, "duration_ms", time.Since(start).Milliseconds())
	}
	cfgStart := time.Now()
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load", "err", err)
		os.Exit(1)
	}
	if err := cfg.ValidateServerSecrets(); err != nil {
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

	// Boot-success line (postmortem 2026-10-14): the Oct 10 CORS outage was
	// diagnosed from this file's absence in the logs. One redacted summary
	// prints every boot so a misconfiguration is visible from the first
	// screen; Config.String masks keys and DSN credentials.
	slog.Info("config loaded", "summary", cfg.String())
	phase("config", cfgStart)

	bootCtx, bootCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer bootCancel()

	// Runtime pool connects with the (potentially non-superuser)
	// app_runtime role. In production this DSN points at the
	// Supabase Supavisor 6543 pooler with simple protocol; the
	// admin DSN points at 5432 direct for DDL.
	poolStart := time.Now()
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
	phase("db_pool", poolStart)
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
	adminStart := time.Now()
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
	phase("admin_pool", adminStart)
	// Migrations are a deploy step on serverless: every cold start
	// replaying the ledger inside an advisory lock costs 150ms+ and
	// serializes parallel boots. Vercel sets RUN_MIGRATIONS=false;
	// long-running hosts keep the default true.
	if cfg.RunMigrations {
		if err := db.MigrateAll(bootCtx, adminPool); err != nil {
			slog.Error("migrate", "err", err)
			os.Exit(1)
		}
		slog.Info("migrations applied")
	} else {
		slog.Info("migrations skipped (RUN_MIGRATIONS=false; run as a deploy step)")
	}

	// Production must not run tenant traffic as a role that can bypass
	// RLS. This runs after migrations (so a virgin self-hosted database
	// has app_runtime) but before Redis, the router, or any request.
	if cfg.Env == "production" || cfg.Env == "staging" {
		role, err := db.VerifyRuntimeRole(bootCtx, pool)
		if err != nil {
			slog.Error("runtime role check", "err", err)
			os.Exit(1)
		}
		slog.Info("runtime role verified", "role", role)
	}

	rdb, err := redisx.NewClient(bootCtx, cfg.RedisURL)
	if err != nil {
		slog.Error("redis connect", "err", err)
		os.Exit(1)
	}
	defer rdb.Close()
	// Traffic-derived SLO histograms: in-process buckets, one EVAL per
	// class per 5-minute interval. Flush before the Redis client closes on
	// shutdown; the periodic path runs from the logging middleware.
	slo.Configure(rdb, "slo")
	defer slo.Flush()
	slog.Info("redis connected")
	agentstools.ReportRDB = rdb // download tokens for chat-generated .xlsx reports
	agentstools.CapRDB = rdb    // per-tenant daily cost cap
	agentstools.CapCallsClient = cfg.CapCallsClient
	agentstools.CapCallsStaff = cfg.CapCallsStaff
	agentstools.CapCallsOwner = cfg.CapCallsOwner
	agentstools.RPDCallsClient = cfg.RPDCallsClient
	agentstools.RPDCallsStaff = cfg.RPDCallsStaff
	agentstools.RPDCallsOwner = cfg.RPDCallsOwner
	usage.CostInPerMtok = cfg.CostInPerMtok
	usage.CostOutPerMtok = cfg.CostOutPerMtok
	agentstools.ReportMaxBytes = cfg.ReportMaxBytes
	agentstools.ConfigureSupabaseReportStore(cfg.SupabaseURL, cfg.SupabaseStorageKey, cfg.ReportBucket)

	cli := &cache.Client{DB: pool, RDB: rdb}
	llmCli := llm.New(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel, cfg.MaxTokensPerReq)

	// Trust boundary for forwarding headers, from config (not a hidden
	// os.Getenv): on Vercel this must list the egress ranges or every
	// client buckets as one IP.
	requestip.ConfigureTrustedProxies(cfg.TrustedProxyCIDRs)

	// CORS origin registry (migration 058): exact origins + suffix rules
	// loaded from the database, merged with the static env list at the
	// router. Boot-load failure is fatal (fail closed — boot already
	// exits without a database). Refresh runs gated like every other
	// background loop; on serverless each cold start reloads instead.
	originsStart := time.Now()
	origins, err := middleware.NewDBResolver(bootCtx, pool)
	if err != nil {
		slog.Error("origins load", "err", err)
		os.Exit(1)
	}
	phase("origins", originsStart)
	ex, su := origins.Counts()
	slog.Info("origins loaded", "exact", ex, "suffix", su)
	if cfg.BackgroundTickers {
		// Background context, NOT bootCtx (30s timeout): the loop must
		// outlive boot. Read-only refresher; dies with the process.
		origins.Start(context.Background(), time.Minute)
	}

	// Per-tenant agent configuration, collected not thrown: each tenant
	// validates independently. A bad tenant is quarantined (fenced with
	// an honest 503) while healthy tenants serve. Only zero healthy
	// tenants -- or a load failure -- is fatal for everyone.
	//
	// Fatal is still the point for shared mistakes: a config that cannot
	// even be listed means no tenant can be evaluated, and the deploy
	// must fail loudly rather than serve an empty world.
	manifestStart := time.Now()
	projectCfgs, badProjects := agentconfig.LoadCollect()
	phase("manifest", manifestStart)
	for slug, err := range features.ValidateProjectDeclarations(projectCfgs) {
		badProjects[slug] = err
		delete(projectCfgs, slug)
	}
	// A compiled project feature without a project.json has no schema,
	// migration ledger, or REST manifest. It must not be mounted from the
	// registry alone; that was a test-only fallback leaking into production.
	for _, slug := range features.UnmanifestedProjects(projectCfgs, badProjects) {
		slog.Error("project features registered without project.json; refusing boot", "project", slug)
		os.Exit(1)
	}
	projects, badTenants, err := agentstools.LoadProjectsSnapshot(projectCfgs, badProjects, agentstools.AllTools(pool))
	if err != nil {
		slog.Error("agent project config unloadable", "err", err)
		os.Exit(1)
	}
	if len(projects) == 0 {
		slog.Error("agent project config: zero healthy tenants")
		os.Exit(1)
	}

	// Entitlement audit is the runtime half of the deploy gate. A deploy
	// whose data backfill did not land (the Centrum failure mode) is caught
	// by cmd/migrate check before promotion; a manual edit afterward is
	// caught here. One bad tenant is fenced, not fatal: healthy tenants keep
	// serving and the admin tenant-health surface names the reason.
	entitlementBad, err := features.AuditProjectEntitlements(bootCtx, pool, projects, badTenants)
	if err != nil {
		slog.Error("tenant entitlement audit failed", "err", err)
		os.Exit(1)
	}
	fences := make(map[string]string, len(badTenants)+len(entitlementBad))
	for slug, reason := range badTenants {
		fences[slug] = "boot: " + reason
	}
	for slug, reason := range entitlementBad {
		if _, already := fences[slug]; already {
			continue
		}
		fences[slug] = "boot: entitlement: " + reason
	}
	for slug, reason := range fences {
		tenanthealth.Quarantine(slug, reason)
		slog.Warn("tenant quarantined at boot", "tenant", slug, "reason", reason)
	}
	// Quarantine is durable (tenant_quarantine) so the Supabase driver
	// honors it too. Boot-derived fences are reconciled, not accumulated: a
	// tenant whose data was repaired is released on the next deploy, while
	// rows written by an operator or a reload failure stay visible.
	reconcileQuarantine(bootCtx, pool, fences)
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
	defer adkRunner.Close()
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
	projecthooks.BookingNotify = func(ctx context.Context, tenantID, to, text string) {
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
	// Push sender is live only with VAPID keys configured; without them
	// the scheduler preserves due rows instead of dead-lettering them.
	// Rows for recipients with no subscribed devices are likewise held
	// (no attempts burned), so going live before any device subscribes
	// is safe. BACKGROUND_TICKERS=false on serverless: the function is
	// destroyed between requests so the 60s ticker would never tick.
	var reminderSend reminder.SendFunc
	if cfg.VAPIDPublicKey != "" && cfg.VAPIDPrivateKey != "" {
		reminderSend = (&reminder.PushSender{
			Pool: adminPool, VAPIDPublic: cfg.VAPIDPublicKey,
			VAPIDPrivate: cfg.VAPIDPrivateKey, Subject: cfg.VAPIDSubject,
		}).Send
	} else {
		slog.Info("reminder push sender disabled (no VAPID keys); preserving pending rows")
	}
	remSched := reminder.New(adminPool, reminderSend)
	if cfg.BackgroundTickers {
		remSched.Start()
	} else {
		slog.Info("reminder ticker disabled on serverless")
	}
	defer remSched.Stop()

	// Agency plane worker (hosting-expiry nudges, testimonial follow-ups,
	// stale-milestone flags, agent-event webhook dispatch). Same gate as
	// every other background loop: long-lived hosts own it, serverless
	// leaves it to pg_cron. Disabled entirely without JWT_SECRET.
	if cfg.AgencyJWTSecret != "" && cfg.BackgroundTickers {
		agencyNotify := agencynotify.New(slog.Default(), cfg.AgencyWhatsapp)
		agencyTelegram := agencynotify.NewTelegram(slog.Default(), "", "")
		agencyWorker := agencyworker.New(slog.Default(), adminPool, agencyNotify, agencyTelegram, cfg.AgencyWhatsapp, agencyworker.BackupConfig{})
		agencyCtx, agencyCancel := context.WithCancel(context.Background())
		defer agencyCancel()
		go agencyWorker.Run(agencyCtx)
	} else {
		slog.Info("agency worker disabled (JWT_SECRET unset or BACKGROUND_TICKERS=false)")
	}

	authH := authapi.New(pool, rdb)
	authH.LoginRatePerMinute = cfg.LoginRateLimitPerMinute
	authH.LoginLockoutFailures = cfg.LoginLockoutFailures
	authH.LoginLockout = time.Duration(cfg.LoginLockoutMinutes) * time.Minute

	router := api.NewRouter(api.Deps{
		Cfg:            cfg,
		Pool:           pool,
		Redis:          rdb,
		Cache:          cli,
		LLM:            llmCli,
		ADK:            adkRunner,
		Auth:           authH,
		Version:        Version,
		AdminPool:      adminPool,
		Origins:        origins,
		ProjectConfigs: projectCfgs,
		BadProjects:    badProjects,
	})

	// Background reclaim for chat-generated .xlsx (not on the
	// download request path). Manifest-driven purge handles durable
	// storage; the legacy local walk covers pre-manifest dev files.
	// Gated like every other background loop: on serverless the
	// function is destroyed between requests, so an ungated sweeper
	// would fire once per cold start and never again.
	reportStop := make(chan struct{})
	if cfg.BackgroundTickers {
		reportsSweeper := &api.Reports{Pool: pool}
		go reportsSweeper.StartSweeper(reportStop)
	} else {
		slog.Info("report sweeper disabled; Supabase owns reclaim")
	}

	// Retention janitor: expired sessions + old sent/dead reminders
	// (migration 027). Hourly, best-effort, on the admin pool (sees all
	// tenants; the deletes are by status/timestamp, not tenant-scoped).
	// Same gate: without it the janitor fires once per cold start on
	// serverless and then never ticks.
	janitorStop := make(chan struct{})
	if cfg.BackgroundTickers {
		go func() {
			t := time.NewTicker(time.Hour)
			defer t.Stop()
			runJanitor := func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer ccancel()
				if _, err := adminPool.Exec(cctx, `DELETE FROM public.staff_sessions WHERE expires_at < now() - interval '7 days'`); err != nil {
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
	} else {
		slog.Info("retention janitor disabled; pg_cron owns retention")
	}

	// Local benchmark profiling only. Production never sets PPROF_ADDR;
	// even if it did, ENV=production refuses to expose the listener. The
	// default mux is dedicated to pprof here, so no application route leaks.
	if addr := os.Getenv("PPROF_ADDR"); addr != "" && cfg.Env != "production" {
		go func() {
			slog.Info("pprof listening", "addr", addr)
			if err := http.ListenAndServe(addr, nil); err != nil && err != http.ErrServerClosed {
				slog.Error("pprof listen", "err", err)
			}
		}()
	}

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
		// 16KB rejects junk headers early; the largest legitimate
		// header here is a session token. ErrorLog routes net/http
		// internal errors through slog instead of the stdlib logger,
		// so accept failures land structured in the log drain.
		MaxHeaderBytes: 16 << 10,
		ErrorLog:       slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
		// TLSConfig left nil for plain HTTP local; PQC default in 1.27 only applies to TLS.
	}

	// Graceful shutdown.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		slog.Info("listening", "addr", srv.Addr, "env", cfg.Env, "model", cfg.LLMModel,
			"boot_ms", time.Since(bootStart).Milliseconds())
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
// tenant_quarantine table through one runtime-pool RPC (migration 091):
//   - rows fenced at this boot are written, so the Supabase driver honors
//     them and a restart does not silently unfence;
//   - stale boot/reload-derived rows are deleted when the current manifest
//     and entitlement are healthy, so a fixed tenant is not fenced forever;
//   - all remaining rows (operator fences, other processes) are loaded into
//     this process's registry.
//
// A boot failure here is fatal: silent divergence between the registry and
// the table would fence tenants for one driver but not the other.
//
// Using the runtime pool means a serverless cold start never opens the
// direct-5432 admin pool just to reconcile fences; that pool stays lazy
// unless an admin request or a long-lived-host worker actually needs it.
func reconcileQuarantine(ctx context.Context, pool *pgxpool.Pool, fences map[string]string) {
	if fences == nil {
		fences = map[string]string{}
	}
	payload, err := json.Marshal(fences)
	if err != nil {
		slog.Error("quarantine payload marshal failed", "err", err)
		os.Exit(1)
	}
	rows, err := pool.Query(ctx,
		`SELECT tenant_id, reason FROM private.sync_tenant_quarantine($1::jsonb)`,
		string(payload))
	if err != nil {
		slog.Error("quarantine sync failed", "err", err)
		os.Exit(1)
	}
	defer rows.Close()
	for rows.Next() {
		var slug, reason string
		if err := rows.Scan(&slug, &reason); err != nil {
			slog.Error("quarantine scan failed", "err", err)
			os.Exit(1)
		}
		if _, have := fences[slug]; !have {
			slog.Warn("tenant quarantined by previous state or operator", "tenant", slug, "reason", reason)
		}
		tenanthealth.Quarantine(slug, reason)
	}
	if err := rows.Err(); err != nil {
		slog.Error("quarantine rows failed", "err", err)
		os.Exit(1)
	}
}

// healthcheck probes the local /healthz endpoint for container
// orchestration. Port follows PORT (default 8080).
func healthcheck() int {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	client := httpx.NewClient(5 * time.Second)
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
