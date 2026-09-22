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

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
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
	return pool
}

// A poison row (undeliverable chat) must flip to dead, not retry forever.
func TestSendOneDeadLettersBadChat(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenant := os.Getenv("TEST_TENANT")
	if tenant == "" {
		tenant = "dev"
	}
	var id string
	err := records.WithTenantTx(ctx, pool, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO agent_reminders (tenant_id, chat_id, text, remind_at)
			 VALUES ($1,'not-a-chat','TEST-DEAD-ROW', now() - interval '1 minute')
			 RETURNING id::text`, tenant).Scan(&id)
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
	s.sendOne(ctx, dueReminder{id: id, tenant: tenant, chat: "not-a-chat", text: "x"})
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
}
