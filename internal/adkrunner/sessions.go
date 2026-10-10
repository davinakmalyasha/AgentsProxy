package adkrunner

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"google.golang.org/adk/v2/session"
)

// sessionTables are the tables ADK's database session service owns. They
// are created by migration 041 (DDL dumped from AutoMigrate output, plus
// grants for the runtime role), never by the app at boot: the runtime pool
// connects as a non-superuser that must not need DDL.
var sessionTables = []string{"sessions", "events", "app_states", "user_states"}

// NewSessionService builds the session store on top of the app pool.
// This is the pgx-native implementation (pgx_sessions.go), proven
// against ADK's own conformance suite and row-compatible with the
// previous gorm-backed store, which it replaced to drop two
// dependencies for zero behavior change.
func NewSessionService(pool *pgxpool.Pool) (session.Service, error) {
	if pool == nil {
		return nil, fmt.Errorf("adkrunner: session pool is required")
	}
	return NewPgxSessionService(pool), nil
}

// VerifySessionTables fails boot loudly when the ADK tables are missing.
// Same philosophy as LoadProjects: a store that is not there produces an
// agent that cannot converse, and that must be a failed deploy, not a 3am
// discovery. One round trip for all four tables.
func VerifySessionTables(ctx context.Context, pool *pgxpool.Pool) error {
	var missing string
	err := pool.QueryRow(ctx,
		`SELECT COALESCE(string_agg(t, ', ' ORDER BY t), '') FROM unnest($1::text[]) AS t
		  WHERE to_regclass('public.' || t) IS NULL`,
		sessionTables).Scan(&missing)
	if err != nil {
		return fmt.Errorf("adkrunner: session table check: %w", err)
	}
	if missing != "" {
		return fmt.Errorf("adkrunner: session tables missing: %s (migrations not applied?)", missing)
	}
	return nil
}
