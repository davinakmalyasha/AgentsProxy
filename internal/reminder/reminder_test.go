// Reminder scheduler tests with a fake sender.
//
// DONE: Phase 5 (telegram agentic) / STANDARDS A
package reminder

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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

// scrubReminders removes every test-scoped row. Tick() runs on the
// admin pool and deliberately delivers across ALL tenants, so one
// test's leftover row (or a row from a previous aborted run) gets
// picked up by the next test and breaks its send count. Tests share
// one Postgres, so isolation is the test's job.
func scrubReminders(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`DELETE FROM agent_reminders
		  WHERE text LIKE 'TEST-%' OR tenant_id LIKE 'rem-%'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM agent_reminders
			  WHERE text LIKE 'TEST-%' OR tenant_id LIKE 'rem-%'`)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM clients WHERE tenant_id LIKE 'rem-%'`)
	})
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
	scrubReminders(t, ctx, pool)

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

// slowSender simulates a Telegram bot that takes ~500ms per send.
// Tick fans out with a semaphore + per-row deadline, so N due rows
// must complete near-parallel — far under N×500ms serial time.
//
// 500ms (not 150ms) deliberately: against a remote database the ~10
// round-trips surrounding the fan-out cost ~450ms of RTT, which swamps
// a 150ms signal entirely (measured 607ms vs a 500ms bound —
// ambiguous). At 500ms/send, serial is 2000ms and parallel lands
// ~950ms, so the bound below separates them with margin to spare.
type slowSender struct {
	mu   sync.Mutex
	sent int
}

func (f *slowSender) Send(_ context.Context, chatID int64, _ string) error {
	time.Sleep(500 * time.Millisecond)
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
	scrubReminders(t, ctx, pool)

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
	// Serial would take ~n*500ms = 2000ms; parallel (semaphore 8) should
	// be one sleep + overhead ≈ well under serial. Bound at 1500ms:
	// serial (2000ms+) cannot pass, parallel (~950ms remote) clears it
	// with ~500ms of RTT headroom.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("elapsed=%v want < 1500ms (proves fan-out; serial would be ~2000ms)", elapsed)
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

// Two drivers (the Go ticker on a long-running host, the Supabase
// pg_cron driver on serverless) must never deliver the same row twice.
// This is the invariant the whole claim design exists to provide, so it
// is asserted directly: fire two Ticks concurrently and require that
// the senders together saw each row exactly once.
func TestScheduler_TwoDriversDeliverEachRowOnce(t *testing.T) {
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
	scrubReminders(t, ctx, pool)

	for _, stmt := range []string{
		`DELETE FROM agent_reminders WHERE tenant_id='rem-two'`,
		`DELETE FROM clients WHERE tenant_id='rem-two'`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM agent_reminders WHERE tenant_id='rem-two'`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM clients WHERE tenant_id='rem-two'`)
	})
	if _, err := pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features)
		 VALUES ('rem-two','rem-two','hash-remt',1000,'active','sports',ARRAY['records','telegram'])`); err != nil {
		t.Fatal(err)
	}
	const n = 8
	for i := 0; i < n; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO agent_reminders (tenant_id, chat_id, text, remind_at)
			 VALUES ('rem-two','12345', $1, now() - interval '1 minute')`,
			fmt.Sprintf("row %d", i)); err != nil {
			t.Fatal(err)
		}
	}

	// One shared counter across both drivers: if a row were delivered
	// twice it would show up as 2 and the total would exceed n.
	shared := &fakeSender{}
	drivers := []*Scheduler{New(pool, shared.Send), New(pool, shared.Send)}
	var wg sync.WaitGroup
	for _, d := range drivers {
		wg.Add(1)
		go func(d *Scheduler) {
			defer wg.Done()
			d.Tick()
		}(d)
	}
	wg.Wait()

	if shared.count() != n {
		t.Fatalf("total sends=%d want %d (a row was delivered more than once)", shared.count(), n)
	}
	seen := map[string]int{}
	for _, s := range shared.sent {
		seen[s]++
	}
	if len(seen) != n {
		t.Fatalf("distinct reminders delivered=%d want %d", len(seen), n)
	}
	for text, count := range seen {
		if count != 1 {
			t.Errorf("reminder %q sent %d times, want 1", text, count)
		}
	}

	// Every row terminal, none left claimed.
	var sent, stuck int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_reminders WHERE tenant_id='rem-two' AND status='sent'`).Scan(&sent); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_reminders WHERE tenant_id='rem-two' AND status='sending'`).Scan(&stuck); err != nil {
		t.Fatal(err)
	}
	if sent != n {
		t.Errorf("sent rows=%d want %d", sent, n)
	}
	if stuck != 0 {
		t.Errorf("rows still claimed=%d want 0 (a driver crashed mid-send)", stuck)
	}
}
