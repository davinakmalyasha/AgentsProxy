// Package notify is the notification queue for the sports vertical.
//
// The design goal: a booking must NEVER depend on WhatsApp being up.
// When a booking is confirmed, the handler enqueues a notification job
// and returns 201 immediately. A drain loop sends via the configured
// Sender, retrying with backoff.
//
// Two senders ship:
//   - WaMeSender: produces a wa.me/<phone>?text=... deep link. Zero
//     credentials, works today. The link is logged and handed to the
//     customer; it opens WhatsApp with the message pre-filled.
//   - (future) Meta/Twilio sender: implement Sender and pass it to
//     Start. The swap is one file.
//
// DURABLE, NOT IN-PROCESS. This used to be a channel drained by a
// worker goroutine. That is correct on a long-running host but silently
// loses every job on a serverless runtime: the handler returns 201, the
// invocation is destroyed, and the worker dies before the send. The job
// is now committed to notify_jobs at enqueue time instead.
//
// The queue is a plain table with a read counter and a visibility
// timeout - the same model pgmq uses, reimplemented so it works on any
// Postgres rather than needing the Supabase-only pgmq extension
// (migration 029). private.claim_notifications() is the SQL twin of
// Drain() for the Supabase driver.
//
// DRIVERS. Start() drains on a ticker and is correct for long-running
// hosts (Laragon, VPS, Railway). On serverless it is off and a Supabase
// Edge Function drains instead, triggered by a Database Webhook
// (instant) with a pg_cron backstop. Mutual exclusion comes from the
// visibility timeout, so a driver that dies before delete makes the row
// reappear rather than losing it.
//
// DONE: Phase 4 (sports vertical, WhatsApp queue) / STANDARDS A, E
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Sender delivers a message to a phone number.
type Sender interface {
	// Send delivers text to the given phone (E.164 or local format).
	// Return an error to trigger a retry.
	Send(ctx context.Context, to, text string) error
}

// Job is one queued notification. Tenant is carried so the row has a
// tenant_id for the RLS policy even though the enqueue runs outside the
// request's transaction.
type Job struct {
	Tenant string `json:"-"`
	To     string `json:"to"`
	Text   string `json:"text"`
}

const (
	// maxAttempts caps claims before the job is archived. Past this the
	// job stops being retried but is kept in pgmq's archive table for
	// inspection; the old in-process queue logged and discarded.
	//
	// Counted from pgmq's own read_ct (which increments on every read),
	// not from a field in the payload. read_ct survives a driver restart
	// for free, and a successful claim deletes the message so it never
	// inflates the count.
	maxAttempts = 3
	// drainEvery is the drain cadence when Start() drives it.
	drainEvery = 5 * time.Second
	// vtInterval is the visibility timeout: how long a claimed job stays
	// hidden from other drivers before it reappears.
	vtInterval = "60 seconds"
	// drainBatch bounds one pass.
	drainBatch = 20
)

// WaMeSender "sends" by generating a wa.me deep link. It can't
// fail, so it never triggers retries. In production, swap for a
// real provider sender (Meta/Twilio).
type WaMeSender struct{}

// Send logs the wa.me link. WhatsApp Business messaging requires a
// provider (Meta Cloud API / Twilio) + business verification; until
// then, a deep link is the zero-credential automated fallback.
func (WaMeSender) Send(_ context.Context, to, text string) error {
	link := "https://wa.me/" + url.PathEscape(to) + "?text=" + url.QueryEscape(text)
	slog.Info("notify wa.me link", "to", to, "link", link)
	return nil
}

// Queue is the durable notification queue backed by notify_jobs.
type Queue struct {
	pool   *pgxpool.Pool
	stopCh chan struct{}
	wg     sync.WaitGroup
	once   sync.Once
}

// New creates a queue over the given pool. Start must be called to
// begin draining on long-running hosts; on serverless, Supabase drives
// the drain and Start is never called.
func New(pool *pgxpool.Pool) *Queue {
	return &Queue{pool: pool, stopCh: make(chan struct{})}
}

// Enqueue durably records a job. Non-blocking by construction: one
// INSERT. Returns an error only on a database failure, which callers
// should log and ignore - a booking must never fail because the
// notification queue was briefly unavailable.
//
// tenantID is recorded for the RLS policy and for support lookups. The
// write goes through the admin pool: the enqueue happens outside the
// booking's transaction, so there is no app.tenant_id to set, and
// notify_jobs is FORCE RLS (migration 029).
//
// Accepted edge case: the booking handler calls this before its
// transaction commits (bookings/feature.go:245 vs the WriteJSON on
// :249), so a crash in that window can leave a notification for a
// booking that never committed. Making it atomic would mean threading a
// pgx.Tx through NotifyFunc at every call site; not worth the blast
// radius for a crash-only window.
func (q *Queue) Enqueue(ctx context.Context, tenantID, to, text string) error {
	if _, err := q.pool.Exec(ctx,
		`INSERT INTO notify_jobs (tenant_id, to_phone, text) VALUES ($1, $2, $3)`,
		tenantID, to, text); err != nil {
		return fmt.Errorf("insert notify job: %w", err)
	}
	return nil
}

// Drain processes up to drainBatch jobs. Sends with the given Sender,
// deleting on success and leaving failed jobs for the visibility
// timeout to reappear. Returns (sent, failed).
//
// At-least-once: a send that succeeds but is followed by a crash
// before delete will be delivered again. WaMeSender has no user-visible
// effect so this is invisible today; a real provider should dedupe on
// its own message id.
func (q *Queue) Drain(ctx context.Context, s Sender) (sent, failed int) {
	rows, err := q.pool.Query(ctx,
		`WITH ready AS (
			SELECT j.id FROM notify_jobs j
			 WHERE j.archived_at IS NULL AND j.vt <= now()
			 ORDER BY j.id
			 LIMIT $1
			 FOR UPDATE SKIP LOCKED
		), claimed AS (
			UPDATE notify_jobs j
			   SET read_ct = j.read_ct + 1, vt = now() + $2::interval
			  FROM ready WHERE j.id = ready.id
			RETURNING j.id, j.read_ct, j.to_phone, j.text
		)
		SELECT c.id, c.read_ct, c.to_phone, c.text FROM claimed c`, drainBatch, vtInterval)
	if err != nil {
		slog.Warn("notify drain read", "err", err)
		return 0, 0
	}
	type claimed struct {
		id     int64
		readCt int32
		job    Job
	}
	var batch []claimed
	for rows.Next() {
		var c claimed
		if err := rows.Scan(&c.id, &c.readCt, &c.job.To, &c.job.Text); err != nil {
			slog.Warn("notify drain scan", "err", err)
			rows.Close()
			return sent, failed
		}
		batch = append(batch, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		slog.Warn("notify drain rows", "err", err)
		return sent, failed
	}

	for _, c := range batch {
		sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := s.Send(sendCtx, c.job.To, c.job.Text)
		cancel()
		// read_ct was already incremented by the claim, and UPDATE
		// ... RETURNING yields the NEW value, so this IS the attempt
		// number. Adding another 1 here double-counts and burns the
		// retry budget one send early.
		attempts := int(c.readCt)
		switch {
		case err == nil:
			q.delete(ctx, c.id)
			sent++
		case attempts >= maxAttempts:
			slog.Error("notify archived after retries", "to", c.job.To,
				"id", c.id, "attempts", attempts, "err", err)
			q.archive(ctx, c.id)
			failed++
		default:
			// Leave it: the visibility timeout brings it back on the
			// next pass with read_ct one higher.
			slog.Warn("notify send failed, will retry", "to", c.job.To,
				"id", c.id, "attempts", attempts, "err", err)
			failed++
		}
	}
	return sent, failed
}

func (q *Queue) delete(ctx context.Context, id int64) {
	if _, err := q.pool.Exec(ctx, `DELETE FROM notify_jobs WHERE id = $1`, id); err != nil {
		// Sent but not deleted: at-least-once becomes duplicate send.
		slog.Error("notify delete failed, may resend", "id", id, "err", err)
	}
}

func (q *Queue) archive(ctx context.Context, id int64) {
	if _, err := q.pool.Exec(ctx,
		`UPDATE notify_jobs SET archived_at = now() WHERE id = $1 AND archived_at IS NULL`, id); err != nil {
		// Doomed job never archived: retried forever, burning budget.
		slog.Error("notify archive failed, job retries forever", "id", id, "err", err)
	}
}

// Start launches the drain loop. Long-running hosts only - on serverless
// Supabase owns the drain.
func (q *Queue) Start(s Sender) {
	q.once.Do(func() {
		q.wg.Add(1)
		go q.worker(s)
	})
}

// Stop signals the drain loop to exit and waits briefly for it.
func (q *Queue) Stop() {
	select {
	case <-q.stopCh:
	default:
		close(q.stopCh)
	}
	done := make(chan struct{})
	go func() { q.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		slog.Warn("notify drain did not stop in time")
	}
}

func (q *Queue) worker(s Sender) {
	defer q.wg.Done()
	t := time.NewTicker(drainEvery)
	defer t.Stop()
	for {
		select {
		case <-q.stopCh:
			return
		case <-t.C:
			q.Drain(context.Background(), s)
		}
	}
}

// MakeWaMeLink is a tiny helper so callers can build the link
// without a Sender (e.g. to echo it in an API response).
func MakeWaMeLink(to, text string) string {
	return "https://wa.me/" + url.PathEscape(to) + "?text=" + url.QueryEscape(text)
}
