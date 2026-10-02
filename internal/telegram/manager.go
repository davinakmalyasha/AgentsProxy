// BotManager supervises thousands of tenant bots (T3).
//
// One row in telegram_bots = one bot = one tenant + one fixed role.
// Transport: webhooks in prod (Telegram pushes to us, zero idle
// connections), per-bot polling fallback for dev (no public URL).
// Every inbound update runs in its own goroutine: N chats on one bot
// are fully parallel; the semaphore only caps total concurrency.
//
// DONE: T3 (BotManager)
package telegram

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/adkrunner"
	"github.com/davinakmalyasha/agents-proxy/internal/tgvault"
)

// ManagedBot is one reconciled bot: decrypted token + live client.
type ManagedBot struct {
	ID            string
	TenantID      string
	Name          string
	Role          string
	Token         string
	WebhookSecret string
	Allow         map[int64]bool
	client        *Bot
	cancel        context.CancelFunc
	fingerprint   string // token_enc+role+allow+status hash for change detection
}

// Manager owns all bots. AdminPool bypasses RLS (must see every tenant).
type Manager struct {
	AdminPool *pgxpool.Pool
	Agent     *adkrunner.Runner
	VaultKey  []byte
	PublicURL string
	PollDev   bool
	Vertical  string

	mu   sync.Mutex
	bots map[string]*ManagedBot
	sem  chan struct{}
}

// NewManager builds a manager (call Sync to load). concurrency caps
// how many updates are handled at once (0 = default 32).
func NewManager(adminPool *pgxpool.Pool, ag *adkrunner.Runner, vaultKey []byte, publicURL string, pollDev bool, concurrency int) *Manager {
	if concurrency <= 0 {
		concurrency = 32
	}
	return &Manager{
		AdminPool: adminPool,
		Agent:     ag,
		VaultKey:  vaultKey,
		PublicURL: strings.TrimRight(publicURL, "/"),
		PollDev:   pollDev,
		Vertical:  "sports",
		bots:      map[string]*ManagedBot{},
		sem:       make(chan struct{}, concurrency),
	}
}

// Sync reconciles DB rows with running bots: starts new, replaces
// changed (token/role/allowlist/status), stops removed/disabled.
// Safe to call on a ticker.
func (m *Manager) Sync(ctx context.Context) {
	if m.AdminPool == nil {
		return
	}
	type row struct {
		ID, TenantID, Name, Role, Secret, Status string
		TokenEnc                                 []byte
		KeyVer                                   int
		Allow                                    []int64
		LastUpdate                               int64
	}
	rows, err := m.AdminPool.Query(ctx,
		`SELECT id::text, tenant_id, name, role, token_enc, key_ver,
		        webhook_secret, allow_chat_ids, status, last_update_id
		   FROM telegram_bots`)
	if err != nil {
		slog.Error("bots sync query", "err", err)
		return
	}
	seen := map[string]row{}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Name, &r.Role, &r.TokenEnc,
			&r.KeyVer, &r.Secret, &r.Allow, &r.Status, &r.LastUpdate); err != nil {
			slog.Error("bots sync scan", "err", err)
			rows.Close()
			return
		}
		seen[r.ID] = r
	}
	rows.Close()

	m.mu.Lock()
	defer m.mu.Unlock()
	// Stop removed or disabled.
	for id, mb := range m.bots {
		r, ok := seen[id]
		if !ok || r.Status != "active" {
			slog.Info("bot stopped", "bot", id, "tenant", mb.TenantID)
			mb.cancel()
			delete(m.bots, id)
		}
	}
	// Start new / replace changed.
	for id, r := range seen {
		if r.Status != "active" {
			continue
		}
		fp := fmt.Sprintf("%x|%s|%v|%s", r.TokenEnc, r.Role, r.Allow, r.Status)
		if mb, ok := m.bots[id]; ok && mb.fingerprint == fp {
			continue // unchanged
		}
		if mb, ok := m.bots[id]; ok {
			mb.cancel()
			delete(m.bots, id)
		}
		tok, err := tgvault.Decrypt(m.VaultKey, r.TokenEnc)
		if err != nil {
			slog.Error("bot decrypt failed", "bot", id, "tenant", r.TenantID,
				"token", tgvault.MaskToken("?:?"), "err", err)
			continue
		}
		allow := map[int64]bool{}
		for _, c := range r.Allow {
			allow[c] = true
		}
		bot := New(string(tok))
		bot.Offset = r.LastUpdate
		ctx, cancel := context.WithCancel(context.Background())
		mb := &ManagedBot{
			ID: id, TenantID: r.TenantID, Name: r.Name, Role: r.Role,
			Token: string(tok), WebhookSecret: r.Secret, Allow: allow,
			client: bot, cancel: cancel, fingerprint: fp,
		}
		m.bots[id] = mb
		slog.Info("bot started", "bot", id, "tenant", r.TenantID, "role", r.Role)
		if m.PublicURL != "" {
			if err := bot.SetWebhook(ctx, m.PublicURL+"/telegram/hook/"+id, r.Secret); err != nil {
				slog.Error("bot setWebhook", "bot", id, "err", err)
			}
		} else if m.PollDev {
			if err := bot.DeleteWebhook(ctx); err != nil {
				slog.Warn("bot deleteWebhook", "bot", id, "err", err)
			}
			go m.pollLoop(ctx, mb)
		} else {
			slog.Warn("bot dormant: no PublicURL and PollDev off", "bot", id)
		}
	}
}

// Count reports managed bots (health/logs).
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.bots)
}

// Close stops all poll loops (graceful shutdown).
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, mb := range m.bots {
		mb.cancel()
		delete(m.bots, id)
	}
}

// lookup finds a running bot (DB fallback on miss).
func (m *Manager) lookup(ctx context.Context, id string) *ManagedBot {
	m.mu.Lock()
	mb, ok := m.bots[id]
	m.mu.Unlock()
	if ok {
		return mb
	}
	// Miss (e.g. created after last Sync): reconcile once, cheaply.
	m.Sync(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bots[id]
}

// ServeHook handles POST /telegram/hook/{id} (webhook transport).
func (m *Manager) ServeHook(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	mb := m.lookup(r.Context(), id)
	if mb == nil {
		http.NotFound(w, r)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Telegram-Bot-Api-Secret-Token")),
		[]byte(mb.WebhookSecret)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	var u Update
	if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
		http.Error(w, "bad update", http.StatusBadRequest)
		return
	}
	// ACK fast (Telegram retries on slow/non-2xx); work continues async.
	w.WriteHeader(http.StatusOK)
	go func() {
		select {
		case m.sem <- struct{}{}:
			defer func() { <-m.sem }()
		case <-time.After(30 * time.Second):
			slog.Warn("hook shed load", "bot", mb.ID)
			return
		}
		// Bound the whole agent run so a stuck provider can't hold a
		// semaphore slot (and shed future updates) indefinitely.
		uCtx, uCancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer uCancel()
		m.handleUpdate(uCtx, mb, u)
	}()
}

// pollLoop is the dev fallback: long-poll one bot, persist offsets
// (no replay storms after restarts).
func (m *Manager) pollLoop(ctx context.Context, mb *ManagedBot) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		updates, err := mb.client.GetUpdates(ctx, 30)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Revoked/dead token: disable so logs don't spam forever.
			if strings.Contains(err.Error(), "401") {
				slog.Error("bot token invalid, disabling", "bot", mb.ID)
				_, _ = m.AdminPool.Exec(context.Background(),
					`UPDATE telegram_bots SET status='disabled' WHERE id=$1::uuid`, mb.ID)
				return
			}
			slog.Warn("bot getUpdates", "bot", mb.ID, "err", err)
			time.Sleep(2 * time.Second)
			continue
		}
		for _, u := range updates {
			if u.Message == nil || u.Message.Text == "" {
				continue
			}
			m.persistOffset(mb)
			m.handleUpdate(ctx, mb, u)
		}
	}
}

func (m *Manager) persistOffset(mb *ManagedBot) {
	_, _ = m.AdminPool.Exec(context.Background(),
		`UPDATE telegram_bots SET last_update_id=$1, updated_at=now() WHERE id=$2::uuid`,
		mb.client.Offset, mb.ID)
}

// handleUpdate routes one message: role + access from the bot row,
// per-chat memory/quota namespaced by bot.
func (m *Manager) handleUpdate(ctx context.Context, mb *ManagedBot, u Update) {
	chatID := u.Message.Chat.ID
	role := mb.Role
	if role != "client" {
		if !mb.Allow[chatID] {
			_ = mb.client.Send(ctx, chatID, "Bot ini private. Minta owner menambahkan chat kamu ke daftar izin.")
			return
		}
	}
	short := mb.ID
	if len(short) > 8 {
		short = short[:8]
	}
	userID := fmt.Sprintf("tg:%s:%d", short, chatID)
	// Stamp with the agentstools copies -- the ones tools actually read.
	// The old loop stamped the agent package's own key types, which are
	// different Go types with the same string value, so context lookups
	// missed and tools (set_reminder's chat delivery) never saw the chat
	// ID on Telegram. Same signature on the runner, role-filtered tools
	// now instead of the shared full set.
	ctx = agentstools.WithChatID(ctx, chatID)
	ctx = agentstools.WithBotID(ctx, mb.ID)
	reply, err := m.Agent.RunWithRole(ctx, mb.TenantID, m.Vertical, userID, role, u.Message.Text)
	if err != nil {
		slog.Warn("bot agent", "bot", mb.ID, "chat", chatID, "err", err)
		_ = mb.client.Send(ctx, chatID, "Sorry, I hit an error. Try again in a moment.")
		return
	}
	// Chat-generated .xlsx → document attachment.
	if path, caption := agentstools.ParseXLSXMarker(reply); path != "" {
		token := strings.TrimPrefix(path, "/v1/reports/")
		if filename, data, derr := agentstools.ResolveReportDownload(token); derr == nil {
			if caption == "" {
				caption = "Laporan Excel 🏸"
			}
			if serr := mb.client.SendDocument(ctx, chatID, filename, data, caption); serr == nil {
				return
			} else {
				slog.Warn("bot sendDocument", "bot", mb.ID, "chat", chatID, "err", serr)
			}
		}
	}
	_ = mb.client.Send(ctx, chatID, reply)
}
