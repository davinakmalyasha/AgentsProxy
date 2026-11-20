package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestNotifyDrivers_PublicExecuteClosed pins the notify surface's
// least-privilege closure. Migration 032 already REVOKEs PUBLIC on the
// whole delivery surface — the three reminder drivers (pre-057), the
// four notify functions, both due-probes and reap_stale_claims — so
// this is a regression pin, not a new fix: DEFERRED.md long claimed the
// notify siblings were "left untouched" after 057; reopened 2026-10-17,
// verified closed on both a fresh chain and prod (no PUBLIC entries in
// pg_proc ACLs), and pinned here so a future rewrite cannot silently
// regress it.
//
// app_runtime keeps its deliberate reap_stale_claims grant (031), and
// delivery_caller keeps the explicit notify-driver grants (the future
// Edge notify driver).
func TestNotifyDrivers_PublicExecuteClosed(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	adminDSN := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	if adminDSN == "" {
		adminDSN = dsn
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	adminPool, err := NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer adminPool.Close()
	if err := Migrate(ctx, adminPool); err != nil {
		t.Fatal(err)
	}

	// app_runtime must be denied on every notify driver + probe. pgx
	// surfaces 42501 lazily, so iterate before checking rows.Err(). The
	// statements are side-effect-safe if a denial ever regresses: notify
	// jobs are empty here and the id arguments match nothing.
	runtimePool, err := NewPool(ctx, toRuntimeDSN(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer runtimePool.Close()
	for _, stmt := range []string{
		`SELECT count(*) FROM private.claim_notifications(1, interval '1 minute')`,
		`SELECT private.delete_notification(1)`,
		`SELECT private.archive_notification(1)`,
		`SELECT private.any_notifications_queued()`,
		`SELECT private.any_reminders_due()`,
	} {
		rows, qerr := runtimePool.Query(ctx, stmt)
		denied := qerr != nil
		if qerr == nil {
			for rows.Next() {
			}
			if rerr := rows.Err(); rerr != nil {
				denied = true
				qerr = rerr
			}
			rows.Close()
		}
		if !denied {
			t.Errorf("app_runtime executed %q, want 42501", stmt)
		} else if !strings.Contains(qerr.Error(), "42501") && !strings.Contains(qerr.Error(), "permission denied") {
			t.Errorf("app_runtime on %q: unexpected error %v (want permission denied)", stmt, qerr)
		}
	}

	// reap_stale_claims stays executable for app_runtime (deliberate
	// 031 grant — regression guard against a blanket revoke).
	var reaped int
	if err := runtimePool.QueryRow(ctx,
		`SELECT private.reap_stale_claims(interval '1 minute')`).Scan(&reaped); err != nil {
		t.Errorf("app_runtime reap_stale_claims: %v (want allowed)", err)
	}

	// delivery_caller keeps EXECUTE on the notify drivers.
	callerPool, err := NewPool(ctx, toCallerDSN(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer callerPool.Close()
	var queued bool
	if err := callerPool.QueryRow(ctx,
		`SELECT private.any_notifications_queued()`).Scan(&queued); err != nil {
		t.Errorf("delivery_caller any_notifications_queued: %v (want allowed)", err)
	}
	var claimed int
	if err := callerPool.QueryRow(ctx,
		`SELECT count(*) FROM private.claim_notifications(1, interval '1 minute')`).Scan(&claimed); err != nil {
		t.Errorf("delivery_caller claim_notifications: %v (want allowed)", err)
	}
}
