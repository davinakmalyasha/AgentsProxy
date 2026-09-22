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
}

// NewPool is the original signature kept for backwards compat.
// Defaults: Min=2, Max=20, extended protocol.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return NewPoolWithOptions(ctx, dsn, NewPoolOptions{MinConns: 2, MaxConns: 20})
}

// NewPoolWithOptions builds a pool with the requested sizing.
// Setting UseSimpleProtocol=true is required when connecting
// through Supabase's Supavisor 6543 transaction-mode pooler,
// which does not support extended-query prepared statements.
func NewPoolWithOptions(ctx context.Context, dsn string, opts NewPoolOptions) (*pgxpool.Pool, error) {
	if opts.MinConns <= 0 {
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
	cfg.HealthCheckPeriod = time.Minute
	if opts.UseSimpleProtocol {
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}
