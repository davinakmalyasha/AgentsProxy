// DONE: PRD 3 (pgxpool singleton) / STANDARDS A, E
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Pool = pgxpool.Pool

// NewPoolOptions lets the caller override the pool size and opt
// into simple-protocol mode (Supabase pooler compatibility).
type NewPoolOptions struct {
	MinConns          int
	MaxConns          int
	UseSimpleProtocol bool
	// MaxConnIdleTime closes idle sessions after this long. Zero keeps
	// the 30m default. Short values matter for pools on session-mode
	// connections with a low server cap (Supabase direct 5432): an idle
	// session still counts against pool_size.
	MaxConnIdleTime time.Duration
	// ApplicationName tags every session in pg_stat_activity / Supabase
	// Query Insights. Empty keeps the pgx default.
	ApplicationName string
}

// NewPool is the original signature kept for backwards compat.
// Defaults: Min=2, Max=20, extended protocol.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return NewPoolWithOptions(ctx, dsn, NewPoolOptions{MinConns: 2, MaxConns: 20})
}

// NewPoolWithOptions builds a pool with the requested sizing.
// MinConns 0 is honored (park nothing): pass -1 for the legacy default.
// Setting UseSimpleProtocol=true is required when connecting
// through Supabase's Supavisor 6543 transaction-mode pooler,
// which does not support extended-query prepared statements.
//
// The pool connects lazily: no Ping runs here, so construction never
// blocks on the network. Callers that need fail-fast (boot with
// RUN_MIGRATIONS=true) get it from Migrate; everyone else surfaces
// the first error on first use, and /healthz reports db:false.
func NewPoolWithOptions(ctx context.Context, dsn string, opts NewPoolOptions) (*pgxpool.Pool, error) {
	if opts.MinConns < 0 {
		opts.MinConns = 2
	}
	if opts.MaxConns <= 0 {
		opts.MaxConns = 20
	}
	if opts.MaxConns < opts.MinConns {
		opts.MaxConns = opts.MinConns
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MinConns = int32(opts.MinConns)
	cfg.MaxConns = int32(opts.MaxConns)
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	if opts.MaxConnIdleTime > 0 {
		cfg.MaxConnIdleTime = opts.MaxConnIdleTime
	}
	cfg.HealthCheckPeriod = time.Minute
	// Bound every session so one pathological plan can never pin a pool
	// connection (and bill Supabase Active CPU) forever. statement_timeout
	// is the backstop for queries that outlive their HTTP handler;
	// idle_in_transaction_session_timeout reclaims connections a handler
	// forgot to commit; lock_timeout keeps DDL/admin from queueing
	// forever behind a held lock. Handlers that legitimately need longer
	// raise their own limit with SET LOCAL (see FetchReportData).
	// ConnectTimeout bounds the TCP+TLS handshake itself (pgx default is
	// none), so a hung network fails the dial instead of the boot.
	cfg.ConnConfig.ConnectTimeout = 10 * time.Second
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	if _, ok := cfg.ConnConfig.RuntimeParams["statement_timeout"]; !ok {
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = "30000"
	}
	if _, ok := cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"]; !ok {
		cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "15000"
	}
	if _, ok := cfg.ConnConfig.RuntimeParams["lock_timeout"]; !ok {
		cfg.ConnConfig.RuntimeParams["lock_timeout"] = "4000"
	}
	if opts.ApplicationName != "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = opts.ApplicationName
	}
	if opts.UseSimpleProtocol {
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return pool, nil
}
