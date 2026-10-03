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
	dsn := os.Getenv("DATABASE_ADMIN_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
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

// A poison row (undeliverable chat) must flip to dead, not retry forever.
//
// The row is seeded already-claimed, matching the state sendOne
// actually receives from Tick(). sendOne scopes its UPDATE by
// claimed_at, so passing a hand-built dueReminder with a zero
// claimedAt would (correctly) match nothing - the claim token is what
// makes a stale send unable to clobber a row the reaper handed back.
func TestSendOneDeadLettersBadChat(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenant := os.Getenv("TEST_TENANT")
	if tenant == "" {
		tenant = "dev"
	}
	var id string
	var claimedAt time.Time
	err := records.WithTenantTx(ctx, pool, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO agent_reminders (tenant_id, chat_id, text, remind_at, status, claimed_at)
			 VALUES ($1,'not-a-chat','TEST-DEAD-ROW', now() - interval '1 minute', 'sending', now())
			 RETURNING id::text, claimed_at`, tenant).Scan(&id, &claimedAt)
	})
	// Unknown tenant (no clients row) → FK error: skip, env has no seed tenant.
	if err != nil {
		t.Skipf("cannot seed (tenant %q?): %v", tenant, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM agent_reminders WHERE id=$1::uuid`, id)
	})
	s := &Scheduler{Pool: pool, Send: func(ctx context.Context, chatID int64, text string) error {
		return errors.New("unreachable in this test")
	}}
	s.sendOne(ctx, dueReminder{id: id, tenant: tenant, recipient: "not-a-chat", text: "x", claimedAt: claimedAt})
	var status string
	err = records.WithTenantTx(ctx, pool, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT status FROM agent_reminders WHERE id=$1::uuid`, id).Scan(&status)
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != "dead" {
		t.Fatalf("status = %q, want dead", status)
	}
	// Verification reads must be tenant-scoped too: agent_reminders is
	// FORCE RLS, so a bare query with no app.tenant_id set correctly
	// matches nothing.
	var claimedNull bool
	if err := records.WithTenantTx(ctx, pool, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT claimed_at IS NULL FROM agent_reminders WHERE id=$1::uuid`, id).Scan(&claimedNull)
	}); err != nil {
		t.Fatal(err)
	}
	if !claimedNull {
		t.Error("claimed_at should be cleared once the row is terminal")
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
