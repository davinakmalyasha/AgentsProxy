// Integration test for dead-lettering (skipped without a database,
// same convention as the sports feature tests).
package reminder

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
)

// testPool prefers the admin DSN so the scrub below can actually see
// and remove test rows. These tests exercise the scheduler's
// cross-tenant claim, which only the admin pool can do anyway; the
// tenant-scoped write paths they assert are still exercised through
// WithTenantTx, so RLS is still in the path.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if dsn == "" {
		dsn = os.Getenv("TEST_DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	scrubReminders(t, ctx, pool)
	return pool
}

// A persistently failing destination must eventually dead-letter the
// row (attempts cap), not retry forever. Routability is enforced at
// creation (recipient_ref required), so delivery failures are transient
// by design and dead arrives via the attempts ladder — there is no
// poison-row shortcut anymore (it died with the Telegram chat IDs).
func TestSendPersistentFailureEventuallyDead(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	const tenant = "rem-push"
	if _, err := pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status)
		 VALUES ('rem-push', $1, 'hash-rempush', 1000000, 'active')
		 ON CONFLICT (tenant_id) DO NOTHING`, tenant); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = pool.Exec(cctx, `DELETE FROM agent_reminders WHERE tenant_id = $1`, tenant)
		_, _ = pool.Exec(cctx, `DELETE FROM clients WHERE tenant_id = $1`, tenant)
	})
	if _, err := pool.Exec(ctx,
		`DELETE FROM agent_reminders WHERE tenant_id = $1`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO agent_reminders (tenant_id, chat_id, text, remind_at, recipient_ref)
		 VALUES ($1, '', 'TEST-PUSH-FAIL', now() - interval '1 minute', 'owner')`, tenant); err != nil {
		t.Fatal(err)
	}
	s := &Scheduler{Pool: pool, Send: func(context.Context, string, string, string) error {
		return errors.New("push service down")
	}}
	var status string
	var attempts int
	for i := 0; i < 7; i++ {
		s.Tick()
		if err := pool.QueryRow(ctx,
			`SELECT status, attempts FROM agent_reminders WHERE tenant_id = $1`, tenant).Scan(&status, &attempts); err != nil {
			t.Fatal(err)
		}
		if status == "dead" {
			break
		}
	}
	if status != "dead" {
		t.Fatalf("status = %q after repeated failures, want dead", status)
	}
	if attempts != 5 {
		t.Errorf("attempts = %d, want 5 (cap, then dead-letter)", attempts)
	}
}

// A send that finished after the reaper window must not be able to
// mark a row the reaper already handed back to the other driver.
func TestSendOneStaleClaimDoesNotComplete(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenant := os.Getenv("TEST_TENANT")
	if tenant == "" {
		tenant = "dev"
	}
	var id string
	err := records.WithTenantTx(ctx, pool, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO agent_reminders (tenant_id, chat_id, text, remind_at, status, claimed_at)
			 VALUES ($1,'12345','TEST-STALE', now() - interval '1 minute', 'sending', now() - interval '30 minutes')
			 RETURNING id::text`, tenant).Scan(&id)
	})
	if err != nil {
		t.Skipf("cannot seed (tenant %q?): %v", tenant, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM agent_reminders WHERE id=$1::uuid`, id)
	})

	// readStatus is tenant-scoped: agent_reminders is FORCE RLS, so a
	// bare query with no app.tenant_id set correctly matches nothing.
	readStatus := func() string {
		t.Helper()
		var s string
		if err := records.WithTenantTx(ctx, pool, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT status FROM agent_reminders WHERE id=$1::uuid`, id).Scan(&s)
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}

	// The reaper hands a 30-minute-old claim back.
	if _, err := pool.Exec(ctx,
		`SELECT private.reap_stale_claims(interval '10 minutes')`); err != nil {
		t.Fatal(err)
	}
	if got := readStatus(); got != "pending" {
		t.Fatalf("after reaper status=%q want pending", got)
	}

	// The slow driver's late completion carries the OLD claim token and
	// must be rejected, leaving the row for whoever holds it now.
	s := &Scheduler{Pool: pool}
	s.sendOne(ctx, dueReminder{
		id: id, tenant: tenant, recipient: "12345", text: "TEST-STALE",
		claimedAt: time.Now().Add(-30 * time.Minute).Truncate(time.Microsecond),
	})
	if got := readStatus(); got != "pending" {
		t.Fatalf("stale send marked the row %q, want pending", got)
	}
}
