// Package reminder is the due-reminder scheduler for the agentic
// layer. A goroutine polls agent_reminders for due rows and sends
// them via the Telegram sender, then marks them sent.
//
// The sender is a func so tests can inject a fake.
//
// DONE: Phase 5 (telegram agentic) / STANDARDS A, E
package reminder

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/davinakmalyasha/agents-proxy/internal/telegram"
	"github.com/davinakmalyasha/agents-proxy/internal/tgvault"
)

// maxConcurrentSends bounds how many reminders a single tick delivers
// in parallel. Telegram per-bot rate limits make fully-serial sends
// safe but slow; a modest fan-out keeps one slow/429 bot from stalling
// every other tenant's reminders while still bounding outbound load.
const maxConcurrentSends = 8

// SendFunc delivers a reminder to a chat (legacy; prefer Scheduler.Key
// + per-row bot tokens, which send through the owning bot).
type SendFunc func(ctx context.Context, chatID int64, text string) error

// Scheduler polls for due reminders.
type Scheduler struct {
	Pool   *pgxpool.Pool
	Send   SendFunc // legacy fallback when a row has no bot
	Key    []byte   // vault master key for per-bot tokens
	Every  time.Duration
	stopCh chan struct{}
}

// New returns a scheduler (not yet running).
func New(pool *pgxpool.Pool, send SendFunc) *Scheduler {
	return &Scheduler{Pool: pool, Send: send, Every: time.Minute, stopCh: make(chan struct{})}
}

// NewWithKey returns a scheduler that delivers through owning bots.
func NewWithKey(pool *pgxpool.Pool, send SendFunc, key []byte) *Scheduler {
	return &Scheduler{Pool: pool, Send: send, Key: key, Every: time.Minute, stopCh: make(chan struct{})}
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
	id, tenant, chat, text string
	botTokenEnc            []byte // nil for legacy rows (use Send fallback)
}

// Tick processes one batch of due reminders.
func (s *Scheduler) Tick() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Find due, pending reminders. The scheduler is internal infra
	// (not a client endpoint), so it is constructed with the admin
	// pool: the SELECT must see every tenant's due rows, which RLS
	// would hide for the runtime role (no tenant GUC). The per-tenant
	// UPDATE still scopes by tenant_id inside WithTenantTx.
	// Bot token joins inline so each reminder goes out its owning bot.
	rows, err := s.Pool.Query(ctx,
		`SELECT r.id::text, r.tenant_id, r.chat_id, r.text, b.token_enc
		   FROM agent_reminders r LEFT JOIN telegram_bots b ON b.id = r.bot_id
		  WHERE r.status='pending' AND r.remind_at <= now() LIMIT 50`)
	if err != nil {
		slog.Warn("reminder query", "err", err)
		return
	}
	defer rows.Close()
	var batch []dueReminder
	for rows.Next() {
		var d dueReminder
		if err := rows.Scan(&d.id, &d.tenant, &d.chat, &d.text, &d.botTokenEnc); err != nil {
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
	// bot can't eat the whole batch context. The batch ctx (10s) is
	// only for the SELECT/UPDATE bookkeeping.
	sem := make(chan struct{}, maxConcurrentSends)
	var wg sync.WaitGroup
	for _, d := range batch {
		d := d
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
	chatID, err := strconv.ParseInt(d.chat, 10, 64)
	if err != nil || chatID == 0 {
		// Permanently undeliverable — dead-letter instead of retrying
		// every minute forever (the old poison loop).
		s.markDead(ctx, d, "bad chat")
		return
	}
	sendErr := s.deliver(ctx, d, chatID)
	if sendErr != nil {
		if errors.Is(sendErr, errHandledDead) {
			return // already dead-lettered inside deliver
		}
		slog.Warn("reminder send", "id", d.id, "err", sendErr)
		s.bumpAttempts(ctx, d)
		return
	}
	// Mark sent inside a tenant tx (RLS lets us update our own row).
	err = records.WithTenantTx(ctx, s.Pool, d.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_reminders SET status='sent' WHERE id=$1::uuid AND tenant_id=$2`, d.id, d.tenant)
		return err
	})
	if err != nil {
		slog.Warn("reminder mark sent", "id", d.id, "err", err)
	}
}

// deliver sends through the owning bot (decrypted per row). Legacy rows
// without a bot fall back to the injected Send func. A Telegram 429
// sleeps retry_after once, then retries once. Permanently unroutable
// rows are dead-lettered and report errHandledDead (not a failure).
func (s *Scheduler) deliver(ctx context.Context, d dueReminder, chatID int64) error {
	if len(d.botTokenEnc) == 0 || len(s.Key) == 0 {
		if s.Send == nil {
			s.markDead(ctx, d, "no bot")
			return errHandledDead
		}
		return s.Send(ctx, chatID, "⏰ Reminder: "+d.text)
	}
	tok, err := tgvault.Decrypt(s.Key, d.botTokenEnc)
	if err != nil {
		slog.Warn("reminder undecryptable, dead-lettering", "id", d.id)
		s.markDead(ctx, d, "bad token")
		return errHandledDead
	}
	bot := telegram.New(string(tok))
	if err := bot.Send(ctx, chatID, "⏰ Reminder: "+d.text); err != nil {
		var rl *telegram.RateError
		if errors.As(err, &rl) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(rl.RetryAfter) * time.Second):
			}
			return bot.Send(ctx, chatID, "⏰ Reminder: "+d.text)
		}
		return err
	}
	return nil
}

var errHandledDead = errors.New("reminder dead-lettered")

// maxAttempts caps send retries; beyond it the row is dead-lettered.
const maxAttempts = 5

func (s *Scheduler) bumpAttempts(ctx context.Context, d dueReminder) {
	_ = records.WithTenantTx(ctx, s.Pool, d.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE agent_reminders SET attempts = attempts + 1,
			 status = CASE WHEN attempts + 1 >= $3 THEN 'dead' ELSE status END
			  WHERE id=$1::uuid AND tenant_id=$2`, d.id, d.tenant, maxAttempts)
		return err
	})
}

func (s *Scheduler) markDead(ctx context.Context, d dueReminder, reason string) {
	slog.Warn("reminder dead-lettered", "id", d.id, "reason", reason)
	_ = records.WithTenantTx(ctx, s.Pool, d.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_reminders SET status='dead' WHERE id=$1::uuid AND tenant_id=$2`, d.id, d.tenant)
		return err
	})
}
