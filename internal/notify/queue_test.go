// Notify queue tests. The queue is table-backed (notify_jobs), so these
// are integration tests against the same Postgres the other packages
// use.
//
// The behaviour worth pinning down here is durability: the queue used
// to be an in-process channel, so these assert a job is committed to
// the database rather than buffered in a goroutine.
//
// DONE: Vercel/Supabase split / STANDARDS A
package notify

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/internal/tenanthealth"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fakeSender struct {
	mu       sync.Mutex
	calls    []string
	failures int // fail this many times before succeeding
}

func (f *fakeSender) Send(_ context.Context, to, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, to+":"+text)
	if f.failures > 0 {
		f.failures--
		return errors.New("provider down")
	}
	return nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSender) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	return f.calls[len(f.calls)-1]
}

// queueCtx returns a live pool, skipping the test when no database is
// configured, and purges the queue so tests do not see each other's jobs
// (the whole package shares one Postgres).
func queueCtx(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("DATABASE_ADMIN_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("DATABASE_URL not set - skipping integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := db.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DELETE FROM notify_jobs`); err != nil {
		t.Fatalf("purge notify_jobs: %v", err)
	}
	return pool, ctx
}

func liveCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notify_jobs WHERE archived_at IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func archivedCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notify_jobs WHERE archived_at IS NOT NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestQueue_EnqueueCommitsToDatabase(t *testing.T) {
	pool, ctx := queueCtx(t)
	q := New(pool)

	if err := q.Enqueue(ctx, "rem-test", "+6281", "hello"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// The whole point of moving off the channel: the job is durable the
	// moment Enqueue returns, independent of any in-process worker.
	if n := liveCount(t, ctx, pool); n != 1 {
		t.Fatalf("queued=%d want 1", n)
	}
}

func TestQueue_EnqueueRecordsTenant(t *testing.T) {
	pool, ctx := queueCtx(t)
	q := New(pool)

	if err := q.Enqueue(ctx, "rem-test", "+6281", "hello"); err != nil {
		t.Fatal(err)
	}
	var tenant string
	if err := pool.QueryRow(ctx, `SELECT tenant_id FROM notify_jobs LIMIT 1`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if tenant != "rem-test" {
		t.Fatalf("tenant_id=%q want rem-test", tenant)
	}
}

func TestQueue_EnqueueNeverDrops(t *testing.T) {
	pool, ctx := queueCtx(t)
	q := New(pool)

	// Far more jobs than any drain batch. Enqueue must never block and
	// never drop: the old channel queue dropped jobs when its buffer
	// filled ("notify queue full, dropping").
	const n = 100
	for i := 0; i < n; i++ {
		if err := q.Enqueue(ctx, "rem-test", "+6281", "flood"); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if got := liveCount(t, ctx, pool); got != n {
		t.Fatalf("queued=%d want %d (no drops allowed)", got, n)
	}
}

func TestQueue_DrainSendsOnceAndDeletes(t *testing.T) {
	pool, ctx := queueCtx(t)
	q := New(pool)
	f := &fakeSender{}

	if err := q.Enqueue(ctx, "rem-test", "+6281", "hello"); err != nil {
		t.Fatal(err)
	}
	sent, failed := q.Drain(ctx, f)
	if sent != 1 || failed != 0 {
		t.Fatalf("sent=%d failed=%d want 1/0", sent, failed)
	}
	if f.count() != 1 {
		t.Fatalf("calls=%d want 1", f.count())
	}
	if !strings.HasPrefix(f.last(), "+6281:") {
		t.Errorf("last=%q", f.last())
	}
	if n := liveCount(t, ctx, pool); n != 0 {
		t.Fatalf("queued=%d want 0 after success", n)
	}
}

// A claimed job is hidden until vt expires, so a second drain right
// after the first must find nothing. This is the property that keeps the
// Go ticker and the Supabase driver from double-sending.
func TestQueue_DrainRespectsVisibilityTimeout(t *testing.T) {
	pool, ctx := queueCtx(t)
	q := New(pool)
	f := &fakeSender{}

	if err := q.Enqueue(ctx, "rem-test", "+6281", "once"); err != nil {
		t.Fatal(err)
	}
	if sent, _ := q.Drain(ctx, f); sent != 1 {
		t.Fatalf("first drain sent=%d want 1", sent)
	}
	// The row is still present (send succeeded but we simulate the
	// delete not having happened) - enqueue again so a live row exists
	// with a future vt, then prove Drain skips it.
	if err := q.Enqueue(ctx, "rem-test", "+6282", "hidden"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE notify_jobs SET vt = now() + interval '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	if sent, _ := q.Drain(ctx, f); sent != 0 {
		t.Fatalf("drain stole a job whose vt had not expired: sent=%d", sent)
	}
	if n := liveCount(t, ctx, pool); n != 1 {
		t.Fatalf("queued=%d want 1 (row must survive)", n)
	}
}

func TestQueue_DrainLeavesFailedJobForRetry(t *testing.T) {
	pool, ctx := queueCtx(t)
	q := New(pool)
	f := &fakeSender{failures: 1} // fail once

	if err := q.Enqueue(ctx, "rem-test", "+6281", "retry me"); err != nil {
		t.Fatal(err)
	}
	sent, failed := q.Drain(ctx, f)
	if sent != 0 || failed != 1 {
		t.Fatalf("sent=%d failed=%d want 0/1", sent, failed)
	}
	if n := liveCount(t, ctx, pool); n != 1 {
		t.Fatalf("queued=%d want 1 (must survive for retry)", n)
	}
}

// A failed job only becomes claimable again after vt expires, which is
// 60s in production. Expire it explicitly rather than sleeping.
func expireVT(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE notify_jobs SET vt = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
}

func TestQueue_DrainArchivesAfterMaxAttempts(t *testing.T) {
	pool, ctx := queueCtx(t)
	q := New(pool)
	f := &fakeSender{failures: 99} // never succeeds

	if err := q.Enqueue(ctx, "rem-test", "+6281", "doomed"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= maxAttempts; i++ {
		expireVT(t, ctx, pool)
		q.Drain(ctx, f)
	}
	if f.count() != maxAttempts {
		t.Fatalf("calls=%d want %d", f.count(), maxAttempts)
	}
	if n := liveCount(t, ctx, pool); n != 0 {
		t.Fatalf("queued=%d want 0 after archive", n)
	}
	// Archived, not dropped: the old queue logged and discarded.
	if n := archivedCount(t, ctx, pool); n != 1 {
		t.Fatalf("archived=%d want 1", n)
	}
}

func TestQueue_DrainLeavesNoStrandedRows(t *testing.T) {
	pool, ctx := queueCtx(t)
	q := New(pool)
	f := &fakeSender{}

	for i := 0; i < 5; i++ {
		if err := q.Enqueue(ctx, "rem-test", "+6281", "batch"); err != nil {
			t.Fatal(err)
		}
	}
	if sent, _ := q.Drain(ctx, f); sent != 5 {
		t.Fatalf("sent=%d want 5", sent)
	}
	var total int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notify_jobs`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Fatalf("rows=%d want 0 (drain must not strand rows)", total)
	}
}

func TestQueue_DrainHoldsQuarantinedTenant(t *testing.T) {
	pool, ctx := queueCtx(t)
	q := New(pool)
	f := &fakeSender{}

	if err := q.Enqueue(ctx, "q-hold", "+6281", "held"); err != nil {
		t.Fatal(err)
	}
	tenanthealth.Quarantine("q-hold", "test")
	defer tenanthealth.Release("q-hold")

	sent, failed := q.Drain(ctx, f)
	if sent != 0 || failed != 0 {
		t.Fatalf("sent=%d failed=%d want 0/0 (quarantined tenant held)", sent, failed)
	}
	if f.count() != 0 {
		t.Fatalf("sender calls=%d want 0", f.count())
	}
	// Job preserved: present, unarchived, claim undone (read_ct back to 0).
	var n, readCt int
	var archivedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(SUM(read_ct),0) FROM notify_jobs WHERE tenant_id='q-hold'`).Scan(&n, &readCt); err != nil {
		t.Fatal(err)
	}
	if n != 1 || readCt != 0 {
		t.Errorf("rows=%d read_ct=%d want 1/0 (hold burns nothing)", n, readCt)
	}
	if err := pool.QueryRow(ctx,
		`SELECT archived_at FROM notify_jobs WHERE tenant_id='q-hold'`).Scan(&archivedAt); err != nil {
		t.Fatal(err)
	}
	if archivedAt != nil {
		t.Error("held job archived")
	}
	// Release → visibility restored (simulate the 5-minute park elapsing)
	// → next drain delivers normally.
	tenanthealth.Release("q-hold")
	if _, err := pool.Exec(ctx, `UPDATE notify_jobs SET vt = now() WHERE tenant_id='q-hold'`); err != nil {
		t.Fatal(err)
	}
	if sent, _ := q.Drain(ctx, f); sent != 1 {
		t.Fatalf("after release sent=%d want 1", sent)
	}
}

func TestWaMeLink_Encodes(t *testing.T) {
	link := MakeWaMeLink("628123", "Booking confirmed: Court 3")
	if !strings.Contains(link, "wa.me/628123?text=") {
		t.Errorf("link=%q", link)
	}
	if strings.Contains(link, " ") {
		t.Errorf("link has raw space: %q", link)
	}
}
