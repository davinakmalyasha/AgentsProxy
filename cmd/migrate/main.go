// Command migrate applies the embedded SQL migrations and exits.
//
// It is the deploy-step runner for environments where the server boots
// with RUN_MIGRATIONS=false. Vercel preview and production refuse boot-time
// migrations; production runs this command through the manual db-migrate
// workflow, gated by the GitHub `production` Environment. Long-running
// hosts (VPS/Railway/compose) may keep boot migrations instead.
//
// Usage:
//
//	DATABASE_URL=... DATABASE_ADMIN_URL=... go run ./cmd/migrate
//	# or: make migrate (loads the local .env; localhost only)
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/davinakmalyasha/agents-proxy/internal/config"
	"github.com/davinakmalyasha/agents-proxy/internal/db"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load", "err", err)
		os.Exit(1)
	}
	dsn := cfg.DatabaseAdminURL
	if dsn == "" {
		dsn = cfg.DatabaseURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool, err := db.NewPoolWithOptions(ctx, dsn, db.NewPoolOptions{
		MinConns:        cfg.DBAdminPoolMin,
		MaxConns:        cfg.DBAdminPoolMax,
		MaxConnIdleTime: time.Minute,
		ApplicationName: "agentsproxy/migrate",
	})
	if err != nil {
		slog.Error("db connect", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		slog.Error("db ping", "err", err)
		os.Exit(1)
	}
	if err := db.MigrateAll(ctx, pool); err != nil {
		slog.Error("migrate", "err", err)
		os.Exit(1)
	}
	slog.Info("migrations applied")
}
