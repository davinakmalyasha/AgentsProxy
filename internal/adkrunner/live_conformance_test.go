package adkrunner

// Baseline conformance: runs ADK's own sessiontestsuite against the
// CURRENT gorm-backed store. Env-gated (needs a live DB); writes only
// under a distinctive app name and cleans up afterwards.
//
// Purpose: prove the store is conformant BEFORE replacing it, so the
// pgx implementation has a green baseline to match rather than a
// debate about what correct means.
//
//	ADK_LIVE_CONFORMANCE=1 go test ./internal/adkrunner/ -run TestLiveConformance -v
import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/sessiontestsuite"
)

const conformanceApp = "suite-conformance"

func conformancePool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	t.Cleanup(func() { wipeConformance(pool) })
	return pool
}

// wipeConformance deletes every row the suite wrote under its app name.
// The suite assumes each setup() call returns an isolated (empty) store --
// true for fresh in-memory maps, false for shared tables -- so setup wipes
// at START too, not just at the end. Without this, one subtest's rows
// break the next subtest's exact counts, and one run's rows break the
// next run.
func wipeConformance(pool *pgxpool.Pool) {
	ctx := context.Background()
	// Events cascade from sessions (FK); app/user states are keyed
	// by app and safe to delete by the same app name.
	_, _ = pool.Exec(ctx, `DELETE FROM sessions WHERE app_name=$1`, conformanceApp)
	_, _ = pool.Exec(ctx, `DELETE FROM app_states WHERE app_name=$1`, conformanceApp)
	_, _ = pool.Exec(ctx, `DELETE FROM user_states WHERE app_name=$1`, conformanceApp)
}

func liveDSN(t *testing.T) string {
	t.Helper()
	if os.Getenv("ADK_LIVE_CONFORMANCE") != "1" {
		t.Skip("opt-in only: writes session rows to the dev database")
	}
	dsn := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if dsn == "" {
		dsn = os.Getenv("TEST_DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("no DSN in environment")
	}
	return dsn
}

// TestLiveConformance_Gorm is the BASELINE: the store as it ships today.
// TestLiveConformance_Pgx runs the same battery against the pgx-native
// store. Both tests green means the swap changes dependencies, not
// behavior.
func TestLiveConformance_Pgx(t *testing.T) {
	dsn := liveDSN(t)
	pool := conformancePool(t, dsn)
	sessiontestsuite.RunServiceTests(t,
		sessiontestsuite.SuiteOptions{
			AppName:                       conformanceApp,
			SupportsUserProvidedSessionID: true,
		},
		func(t *testing.T) session.Service {
			wipeConformance(pool)
			return NewPgxSessionService(pool)
		},
	)
}
