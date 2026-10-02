package adkrunner

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"google.golang.org/adk/v2/session"
	adkdatabase "google.golang.org/adk/v2/session/database"
)

// sessionTables are the tables ADK's database session service owns. They
// are created by migration 041 (DDL dumped from AutoMigrate output, plus
// grants for the runtime role), never by the app at boot: the runtime pool
// connects as a non-superuser that must not need DDL.
var sessionTables = []string{"sessions", "events", "app_states", "user_states"}

// NewSessionService builds the ADK session store on top of the app pool.
// stdlib.OpenDBFromPool shares the pool's connections rather than opening
// new ones, so session reads/writes cost no extra connections.
func NewSessionService(pool *pgxpool.Pool) (session.Service, error) {
	gdb, err := gorm.Open(postgres.New(postgres.Config{
		Conn: stdlib.OpenDBFromPool(pool),
	}), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("adkrunner: session gorm: %w", err)
	}
	svc, err := adkdatabase.NewSessionServiceFromDB(gdb)
	if err != nil {
		return nil, fmt.Errorf("adkrunner: session service: %w", err)
	}
	return svc, nil
}

// VerifySessionTables fails boot loudly when the ADK tables are missing.
// Same philosophy as LoadProjects: a store that is not there produces an
// agent that cannot converse, and that must be a failed deploy, not a 3am
// discovery.
func VerifySessionTables(ctx context.Context, pool *pgxpool.Pool) error {
	for _, t := range sessionTables {
		var ok bool
		if err := pool.QueryRow(ctx,
			`SELECT to_regclass($1) IS NOT NULL`, "public."+t).Scan(&ok); err != nil {
			return fmt.Errorf("adkrunner: session table check %s: %w", t, err)
		}
		if !ok {
			return fmt.Errorf("adkrunner: session table public.%s is missing (migrations not applied?)", t)
		}
	}
	return nil
}
