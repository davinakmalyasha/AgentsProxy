package adkrunner

// Store-level tenant isolation for the pgx session service.
//
// app_name IS the tenant slug and every query in pgx_sessions.go filters on
// it, but RLS on these tables is USING(true) (migration 041), so the
// database does not enforce anything — a missing app_name filter in a new
// query would silently read every tenant's sessions. DB-level enforcement
// via a tx-scoped GUC was evaluated and rejected: the hot reads (Get/List
// per agent turn) run pool-direct, and wrapping each in BEGIN + SET LOCAL +
// COMMIT adds round trips on the exact path the perf pass just optimized.
// This test is the enforcement instead: it fails if any store operation
// ever leaks across app names.
import (
	"context"
	"os"
	"testing"

	"google.golang.org/adk/v2/session"

	"github.com/davinakmalyasha/agents-proxy/internal/db"
)

const (
	isoAppA = "iso-tenant-a"
	isoAppB = "iso-tenant-b"
)

func isolationStore(t *testing.T) session.Service {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping session isolation test")
	}
	adminDSN := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if adminDSN == "" {
		adminDSN = dsn
	}
	ctx := context.Background()
	admin, err := db.NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	if err := db.Migrate(ctx, admin); err != nil {
		t.Fatal(err)
	}
	pool, err := db.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	t.Cleanup(func() {
		for _, app := range []string{isoAppA, isoAppB} {
			_, _ = admin.Exec(ctx, `DELETE FROM sessions WHERE app_name=$1`, app)
			_, _ = admin.Exec(ctx, `DELETE FROM app_states WHERE app_name=$1`, app)
			_, _ = admin.Exec(ctx, `DELETE FROM user_states WHERE app_name=$1`, app)
		}
	})
	return NewPgxSessionService(pool)
}

func TestSessionStore_TenantIsolation(t *testing.T) {
	svc := isolationStore(t)
	ctx := context.Background()

	mk := func(app, user string) string {
		t.Helper()
		resp, err := svc.Create(ctx, &session.CreateRequest{AppName: app, UserID: user})
		if err != nil {
			t.Fatalf("create %s/%s: %v", app, user, err)
		}
		return resp.Session.ID()
	}
	sidA := mk(isoAppA, "u1")
	sidB := mk(isoAppB, "u1")

	// Get under the wrong app must not find the session.
	if _, err := svc.Get(ctx, &session.GetRequest{AppName: isoAppB, UserID: "u1", SessionID: sidA}); err == nil {
		t.Error("Get(appB, sidA) succeeded, want not-found")
	}
	getA, err := svc.Get(ctx, &session.GetRequest{AppName: isoAppA, UserID: "u1", SessionID: sidA})
	if err != nil {
		t.Fatalf("Get(appA, sidA): %v", err)
	}
	if getA.Session.ID() != sidA {
		t.Errorf("Get(appA) id=%q want %q", getA.Session.ID(), sidA)
	}

	// List must be scoped to the requesting app.
	listA, err := svc.List(ctx, &session.ListRequest{AppName: isoAppA})
	if err != nil {
		t.Fatalf("List(appA): %v", err)
	}
	for _, s := range listA.Sessions {
		if s.AppName() != isoAppA {
			t.Errorf("List(appA) leaked session from app %q", s.AppName())
		}
	}
	if len(listA.Sessions) != 1 || listA.Sessions[0].ID() != sidA {
		t.Errorf("List(appA) = %d sessions, want exactly sidA", len(listA.Sessions))
	}
	listB, err := svc.List(ctx, &session.ListRequest{AppName: isoAppB})
	if err != nil {
		t.Fatalf("List(appB): %v", err)
	}
	if len(listB.Sessions) != 1 || listB.Sessions[0].ID() != sidB {
		t.Errorf("List(appB) = %d sessions, want exactly sidB", len(listB.Sessions))
	}

	// Delete under the wrong app must leave the session intact.
	if err := svc.Delete(ctx, &session.DeleteRequest{AppName: isoAppB, UserID: "u1", SessionID: sidA}); err != nil {
		t.Fatalf("cross-app Delete: %v", err)
	}
	if _, err := svc.Get(ctx, &session.GetRequest{AppName: isoAppA, UserID: "u1", SessionID: sidA}); err != nil {
		t.Errorf("Get(appA, sidA) after cross-app Delete: %v (row was deleted)", err)
	}

	// Empty app names fail closed at every entry point.
	if _, err := svc.Create(ctx, &session.CreateRequest{UserID: "u1"}); err == nil {
		t.Error("Create without app_name succeeded, want error")
	}
	if _, err := svc.Get(ctx, &session.GetRequest{UserID: "u1", SessionID: sidA}); err == nil {
		t.Error("Get without app_name succeeded, want error")
	}
	if _, err := svc.List(ctx, &session.ListRequest{}); err == nil {
		t.Error("List without app_name succeeded, want error")
	}
	if err := svc.Delete(ctx, &session.DeleteRequest{UserID: "u1", SessionID: sidA}); err == nil {
		t.Error("Delete without app_name succeeded, want error")
	}
}
