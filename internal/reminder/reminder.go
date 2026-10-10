// Package reminder is the due-reminder scheduler for the agentic
// layer. A goroutine polls agent_reminders for due rows and sends
// them through the configured delivery transport, then marks them sent.
//
// Telegram delivery was deleted. Until the PWA delivery transport lands,
// a scheduler without a sender preserves pending rows instead of
// claiming or dead-lettering them.
//
// The sender is a func so tests can inject a fake and the future PWA
// transport can plug in here.
//
// TWO DRIVERS, ONE CLAIM. This ticker is the driver for long-running
// hosts (Laragon, VPS, Railway). On serverless (Vercel) it is disabled
// via BACKGROUND_TICKERS=false and a Supabase Edge Function driven by
// pg_cron delivers instead. Every path claims a row by atomically
// flipping status to 'sending' and stamping claimed_at; drivers only
// read status='pending', so the other driver can never see a claimed
// row. private.reap_stale_claims() hands back rows orphaned by a crash.
// See migrations 030_reminder_claim.sql and 031_delivery_roles.sql.
//
// DONE: Phase 5 (reminders) / STANDARDS A, E
package reminder

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/tenanthealth"
)

// maxConcurrentSends bounds how many reminders a single tick delivers
// in parallel. A modest fan-out keeps one slow destination from stalling
// every other tenant's reminders while still bounding outbound load.
const maxConcurrentSends = 8

// SendFunc delivers a reminder to a recipient. The recipient is currently
// the legacy chat identifier; the PWA transport will replace it with a
// user/device recipient.
type SendFunc func(ctx context.Context, recipientID int64, text string) error

// Scheduler polls for due reminders.
type Scheduler struct {
	Pool   *pgxpool.Pool
	Send   SendFunc // nil until the PWA delivery transport lands
	Every  time.Duration
	stopCh chan struct{}
}

// New returns a scheduler (not yet running).
func New(pool *pgxpool.Pool, send SendFunc) *Scheduler {
	return &Scheduler{Pool: pool, Send: send, Every: time.Minute, stopCh: make(chan struct{})}
}

// Start launches the polling goroutine.
func (s *Scheduler) Start() {
	go s.loop()
}

// Stop signals the goroutine to exit.
func (s *Scheduler) Stop() {
	close(s.stopCh)
}

func (s *Scheduler) loop() {
	ticker := time.NewTicker(s.Every)
	defer ticker.Stop()
	s.Tick() // fire once immediately
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.Tick()
		}
	}
}

type dueReminder struct {
	id, tenant, recipient, text string
	claimedAt                   time.Time
}

// Tick processes one batch of due reminders.
func (s *Scheduler) Tick() {
	if s.Send == nil {
		// No delivery transport is configured. Do not claim rows: claiming
		// would move them to 'sending' and the missing sender would then
		// dead-letter reminders that a future PWA transport could deliver.
		slog.Warn("reminder delivery unavailable; preserving pending rows until PWA transport lands")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// CLAIM due, pending reminders. The UPDATE in the same statement
	// flips status to 'sending' and stamps claimed_at, so the claim is
	// atomic with the read and the other driver cannot see these rows.
	// FOR UPDATE SKIP LOCKED keeps two concurrent ticks (or a tick
	// racing the Supabase driver) from colliding.
	//
	// The scheduler is internal infra (not a client endpoint), so it is
	// constructed with the admin pool: the SELECT must see every
	// tenant's due rows, which FORCE ROW LEVEL SECURITY would hide for
	// the runtime role (no tenant GUC). The per-row completion UPDATEs
	// still scope by claimed_at so a stale send cannot clobber a row
	// the reaper handed back.
	//
	// Claim rows without joining a delivery transport: transport-specific
	// addressing belongs to the sender, not the claim.
	rows, err := s.Pool.Query(ctx,
		`WITH due AS (
			SELECT r.id FROM agent_reminders r
			 WHERE r.status = 'pending' AND r.remind_at <= now()
			 ORDER BY r.remind_at
			 LIMIT $1
			 FOR UPDATE SKIP LOCKED
		), claimed AS (
			UPDATE agent_reminders r
			   SET status = 'sending', claimed_at = clock_timestamp()
			  FROM due WHERE r.id = due.id
			RETURNING r.id, r.tenant_id, r.chat_id, r.text, r.attempts,
			          r.claimed_at
		)
		SELECT c.id::text, c.tenant_id, c.chat_id, c.text, c.claimed_at
		  FROM claimed c`, 50)
	if err != nil {
		slog.Warn("reminder query", "err", err)
		return
	}
	defer rows.Close()
	var batch []dueReminder
	for rows.Next() {
		var d dueReminder
		if err := rows.Scan(&d.id, &d.tenant, &d.recipient, &d.text, &d.claimedAt); err != nil {
			slog.Warn("reminder scan", "err", err)
			return
		}
		batch = append(batch, d)
	}
	if err := rows.Err(); err != nil {
		slog.Warn("reminder rows", "err", err)
		return
	}

	// Deliver concurrently, each row under its own deadline so a slow
	// destination can't eat the whole batch context. The batch ctx (10s) is
	// only for the SELECT/UPDATE bookkeeping.
	sem := make(chan struct{}, maxConcurrentSends)
	var wg sync.WaitGroup
	for _, d := range batch {
		d := d
		// Quarantine fence: a fenced tenant's rows wait in pending. The
		// claim already flipped them to sending, so release the claim
		// without burning attempts — never send, never dead-letter.
		if q, reason := tenanthealth.Check(d.tenant); q {
			slog.Warn("quarantined tenant reminder held", "tenant", d.tenant, "reason", reason, "id", d.id)
			s.releaseClaim(ctx, d)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			rowCtx, rowCancel := context.WithTimeout(ctx, 8*time.Second)
			defer rowCancel()
			s.sendOne(rowCtx, d)
		}()
	}
	wg.Wait()
}

func (s *Scheduler) sendOne(ctx context.Context, d dueReminder) {
	recipientID, err := strconv.ParseInt(d.recipient, 10, 64)
	if err != nil || recipientID == 0 {
		// Permanently undeliverable — dead-letter instead of retrying
		// every minute forever (the old poison loop).
		s.markDead(ctx, d, "bad recipient")
		return
	}
	sendErr := s.deliver(ctx, d, recipientID)
	if sendErr != nil {
		if errors.Is(sendErr, errHandledDead) {
			return // already dead-lettered inside deliver
		}
		slog.Warn("reminder send", "id", d.id, "err", sendErr)
		s.bumpAttempts(ctx, d)
		return
	}
	// Mark sent. Scoped by claimed_at: if the reaper already handed
	// this row back to another driver, the update matches nothing and we
	// must NOT report success.
	//
	// Single statement, no transaction: this is one PK update, and the
	// scheduler runs on the admin pool (see Tick), so no tenant GUC is
	// needed — the explicit tenant_id predicate scopes the row. A
	// transaction here would cost BEGIN + set_config + COMMIT (3 wasted
	// round trips) per reminder, 4× the work on the size-3 admin pool.
	tag, err := s.Pool.Exec(ctx,
		`UPDATE agent_reminders SET status='sent', claimed_at=NULL
		  WHERE id=$1::uuid AND tenant_id=$2 AND status='sending' AND claimed_at=$3`,
		d.id, d.tenant, d.claimedAt)
	if err != nil {
		slog.Warn("reminder mark sent failed, will retry", "id", d.id, "err", err)
		return
	}
	if tag.RowsAffected() == 0 {
		slog.Warn("reminder claim lost before mark sent", "id", d.id)
	}
}

// deliver sends through the configured transport. Without a sender it
// returns an error without dead-lettering, so Tick's early return (and
// the claim-scoped updates) preserve rows until PWA delivery lands.
func (s *Scheduler) deliver(ctx context.Context, d dueReminder, recipientID int64) error {
	if s.Send == nil {
		return errors.New("reminder delivery unavailable")
	}
	return s.Send(ctx, recipientID, "⏰ Reminder: "+d.text)
}

var errHandledDead = errors.New("reminder dead-lettered")

// maxAttempts caps send retries; beyond it the row is dead-lettered.
const maxAttempts = 5

// bumpAttempts records a transient send failure and releases the claim
// so the row becomes visible to the next tick. Mirrors the retired
// private.fail_reminder (dropped in 055) including maxAttempts = 5.
//
// The claim design forces one change from the original: the old query
// left `status` untouched because rows were never claimed. A claimed
// row must be reset to 'pending' explicitly or it stays invisible to
// every driver forever.
func (s *Scheduler) bumpAttempts(ctx context.Context, d dueReminder) {
	// Single statement (see mark-sent above for why no transaction).
	tag, err := s.Pool.Exec(ctx,
		`UPDATE agent_reminders
		    SET attempts = attempts + 1,
		        status = CASE WHEN attempts + 1 >= $4 THEN 'dead' ELSE 'pending' END,
		        claimed_at = NULL
		  WHERE id=$1::uuid AND tenant_id=$2 AND status='sending' AND claimed_at=$3`,
		d.id, d.tenant, d.claimedAt, maxAttempts)
	if err != nil {
		slog.Warn("reminder bump failed, will retry", "id", d.id, "err", err)
		return
	}
	if tag.RowsAffected() == 0 {
		slog.Warn("reminder claim lost before bump", "id", d.id)
	}
}

// releaseClaim hands a claimed row back to pending without touching
// attempts. Used for quarantine holds: the row must wait, not retry.
// Single statement (see mark-sent above for why no transaction).
func (s *Scheduler) releaseClaim(ctx context.Context, d dueReminder) {
	if _, err := s.Pool.Exec(ctx,
		`UPDATE agent_reminders SET status='pending', claimed_at=NULL
		  WHERE id=$1::uuid AND tenant_id=$2 AND status='sending' AND claimed_at=$3`,
		d.id, d.tenant, d.claimedAt); err != nil {
		slog.Warn("reminder release failed, reaper will recover", "id", d.id, "err", err)
	}
}

func (s *Scheduler) markDead(ctx context.Context, d dueReminder, reason string) {
	// Dead-lettering is permanent data loss signal: it pages.
	slog.Error("reminder dead-lettered", "id", d.id, "reason", reason)
	// Single statement (see mark-sent above for why no transaction).
	tag, err := s.Pool.Exec(ctx,
		`UPDATE agent_reminders SET status='dead', claimed_at=NULL
		  WHERE id=$1::uuid AND tenant_id=$2 AND status='sending' AND claimed_at=$3`,
		d.id, d.tenant, d.claimedAt)
	if err != nil {
		slog.Error("reminder dead-letter write failed", "id", d.id, "err", err)
		return
	}
	if tag.RowsAffected() == 0 {
		slog.Warn("reminder claim lost before dead-letter", "id", d.id)
	}
}
