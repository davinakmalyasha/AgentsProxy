// Package notify is the notification queue for the sports vertical.
//
// The design goal: a booking must NEVER depend on WhatsApp being up.
// When a booking is confirmed, the handler enqueues a notification
// job (non-blocking) and returns 201 immediately. A background
// worker drains the queue, sending via the configured Sender with
// retry + exponential backoff.
//
// Two senders ship:
//   - WaMeSender: produces a wa.me/<phone>?text=... deep link. Zero
//     credentials, works today. The link is logged and handed to the
//     customer; it opens WhatsApp with the message pre-filled.
//   - (future) Meta/Twilio sender: implement Sender and pass it to
//     Start. The swap is one file.
//
// The queue is in-process (channel + worker goroutine). If the server
// ever scales to multiple instances, swap the queue for Redis
// streams — the Sender interface isolates that change.
//
// DONE: Phase 4 (sports vertical, WhatsApp queue) / STANDARDS A, E
package notify

import (
	"context"
	"log/slog"
	"net/url"
	"sync"
	"time"
)

// Sender delivers a message to a phone number.
type Sender interface {
	// Send delivers text to the given phone (E.164 or local format).
	// Return an error to trigger a retry.
	Send(ctx context.Context, to, text string) error
}

// Job is one queued notification.
type Job struct {
	To   string
	Text string
}

const (
	maxAttempts = 3
	// baseBackoff is the initial retry delay; each attempt doubles it.
	baseBackoff = time.Second
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

// Queue is the in-process notification queue.
type Queue struct {
	ch     chan Job
	stopCh chan struct{}
	wg     sync.WaitGroup
	once   sync.Once
}

// New creates a queue with the given buffer size. Start must be
// called to begin draining.
func New(buffer int) *Queue {
	if buffer < 1 {
		buffer = 16
	}
	return &Queue{
		ch:     make(chan Job, buffer),
		stopCh: make(chan struct{}),
	}
}

// Start launches the worker goroutine draining q into s.
func (q *Queue) Start(s Sender) {
	q.once.Do(func() {
		q.wg.Add(1)
		go q.worker(s)
	})
}

// Enqueue adds a job. Non-blocking: if the buffer is full the job is
// dropped and logged (never blocks a booking request).
func (q *Queue) Enqueue(j Job) {
	select {
	case q.ch <- j:
	default:
		slog.Warn("notify queue full, dropping", "to", j.To)
	}
}

// Stop signals the worker and waits for it to drain (with a cap).
func (q *Queue) Stop() {
	close(q.stopCh)
	done := make(chan struct{})
	go func() { q.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		slog.Warn("notify worker did not stop in time")
	}
}

func (q *Queue) worker(s Sender) {
	defer q.wg.Done()
	for {
		select {
		case <-q.stopCh:
			return
		case j := <-q.ch:
			q.sendWithRetry(context.Background(), s, j)
		}
	}
}

func (q *Queue) sendWithRetry(ctx context.Context, s Sender, j Job) {
	delay := baseBackoff
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := s.Send(ctx, j.To, j.Text); err == nil {
			return
		} else {
			slog.Warn("notify send failed", "to", j.To, "attempt", attempt, "err", err)
		}
		if attempt < maxAttempts {
			select {
			case <-time.After(delay):
			case <-q.stopCh:
				return
			}
			delay *= 2
		}
	}
	slog.Error("notify send gave up", "to", j.To)
}

// MakeWaMeLink is a tiny helper so callers can build the link
// without a Sender (e.g. to echo it in an API response).
func MakeWaMeLink(to, text string) string {
	return "https://wa.me/" + url.PathEscape(to) + "?text=" + url.QueryEscape(text)
}
