// Reminder scheduler tests with a fake sender.
//
// DONE: Phase 5 (telegram agentic) / STANDARDS A
package reminder

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/jackc/pgx/v5"
)

type fakeSender struct {
	mu   sync.Mutex
	sent []string
}

func (f *fakeSender) Send(_ context.Context, chatID int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, text)
	return nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func TestScheduler_SendsDue(t *testing.T) {
	dsn := os.Getenv("DATABASE_ADMIN_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	// Seed a client + a due + a future reminder (admin pool, RLS off).
	for _, stmt := range []string{
		`DELETE FROM agent_reminders WHERE tenant_id='rem-test'`,
		`DELETE FROM clients WHERE tenant_id='rem-test'`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features)
		 VALUES ('rem-test','rem-test','hash-rem',1000,'active','sports',ARRAY['records','telegram'])`); err != nil {
		t.Fatal(err)
	}
	var dueID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO agent_reminders (tenant_id, chat_id, text, remind_at) VALUES ('rem-test','12345','order shuttlecocks', now() - interval '1 minute') RETURNING id::text`,
	).Scan(&dueID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO agent_reminders (tenant_id, chat_id, text, remind_at) VALUES ('rem-test','12345','future thing', now() + interval '1 hour')`); err != nil {
		t.Fatal(err)
	}

	fake := &fakeSender{}
	s := New(pool, fake.Send)
	s.Tick() // one synchronous batch

	if fake.count() != 1 {
		t.Fatalf("sent=%d want 1 (only the due reminder)", fake.count())
	}
	// The due one is marked sent.
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM agent_reminders WHERE id=$1::uuid`, dueID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "sent" {
		t.Errorf("status=%s want sent", status)
	}
	// The future one is still pending.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_reminders WHERE tenant_id='rem-test' AND status='pending'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("pending=%d want 1", n)
	}

	// Cleanup.
	_ = records.WithTenantTx(ctx, pool, "rem-test", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM agent_reminders WHERE tenant_id='rem-test'`)
		return err
	})
	_, _ = pool.Exec(ctx, `DELETE FROM clients WHERE tenant_id='rem-test'`)
}

// slowSender simulates a Telegram bot that takes ~150ms per send.
// Tick fans out with a semaphore + per-row deadline, so N due rows
// must complete near-parallel — far under N×150ms serial time.
type slowSender struct {
	mu   sync.Mutex
	sent int
}

func (f *slowSender) Send(_ context.Context, chatID int64, _ string) error {
	time.Sleep(150 * time.Millisecond)
	f.mu.Lock()
	f.sent++
	f.mu.Unlock()
	return nil
}

func TestScheduler_DeliversConcurrently(t *testing.T) {
	dsn := os.Getenv("DATABASE_ADMIN_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	for _, stmt := range []string{
		`DELETE FROM agent_reminders WHERE tenant_id='rem-par'`,
		`DELETE FROM clients WHERE tenant_id='rem-par'`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features)
		 VALUES ('rem-par','rem-par','hash-remp',1000,'active','sports',ARRAY['records','telegram'])`); err != nil {
		t.Fatal(err)
	}
	const n = 4
	for i := 0; i < n; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO agent_reminders (tenant_id, chat_id, text, remind_at) VALUES ('rem-par','12345', $1, now() - interval '1 minute')`,
			"par "+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}

	fake := &slowSender{}
	s := New(pool, fake.Send)
	start := time.Now()
	s.Tick()
	elapsed := time.Since(start)

	if fake.sent != n {
		t.Fatalf("sent=%d want %d", fake.sent, n)
	}
	// Serial would take ~n*150ms = 600ms; parallel (semaphore 8) should
	// be one sleep + overhead ≈ well under 2x a single send.
	if elapsed > 500*time.Millisecond {
		t.Errorf("elapsed=%v want < 500ms (proves fan-out; serial would be ~600ms)", elapsed)
	}
	// All marked sent.
	var sent int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_reminders WHERE tenant_id='rem-par' AND status='sent'`).Scan(&sent); err != nil {
		t.Fatal(err)
	}
	if sent != n {
		t.Errorf("sent rows=%d want %d", sent, n)
	}

	_ = records.WithTenantTx(ctx, pool, "rem-par", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM agent_reminders WHERE tenant_id='rem-par'`)
		return err
	})
	_, _ = pool.Exec(ctx, `DELETE FROM clients WHERE tenant_id='rem-par'`)
}
