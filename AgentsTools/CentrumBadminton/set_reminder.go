// Package centrumbadminton implements Centrum Badminton's agent tools.
//
// One file per tool. Each file is self-contained: the Args shape, the
// description the model sees, and the Run function that executes it.
package centrumbadminton

import (
	"context"
	"encoding/json"
	"fmt"
	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"strings"
	"time"
)

func init() { agentstools.Register("set_reminder", setReminderTool) }

func setReminderTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name: "set_reminder", Mutating: true,
		Description: "Set a reminder delivered via Telegram. Args: {text, remind_at (YYYY-MM-DD HH:MM WIB)}. Returns the reminder id. Only works inside a Telegram chat.",
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				Text     string `json:"text"`
				RemindAt string `json:"remind_at"`
			}
			if err := json.Unmarshal(args, &p); err != nil || strings.TrimSpace(p.Text) == "" {
				return "error: text and remind_at required"
			}
			text := strings.TrimSpace(p.Text)
			if len(text) > 500 {
				return "error: text too long (max 500 chars)"
			}
			// Delivery is Telegram-only: without the sender's chat there
			// is nowhere to send. Never store undeliverable rows.
			chatID := agentstools.ChatIDFromCtx(ctx)
			if chatID == "" {
				return "error: reminders can only be set from Telegram chat Ã¢â‚¬â€ I don't know where to send this"
			}
			when, err := time.ParseInLocation("2006-01-02 15:04", strings.TrimSpace(p.RemindAt), agentstools.WibLoc())
			if err != nil {
				return "error: remind_at must be YYYY-MM-DD HH:MM"
			}
			if when.Before(agentstools.WibNow().Add(-time.Minute)) {
				return "error: remind_at is in the past"
			}
			if when.After(agentstools.WibNow().AddDate(1, 0, 0)) {
				return "error: remind_at too far ahead (max 1 year)"
			}
			var id string
			botID := agentstools.BotIDFromCtx(ctx)
			err = records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				if botID == "" {
					return tx.QueryRow(ctx,
						`INSERT INTO agent_reminders (tenant_id, chat_id, text, remind_at) VALUES ($1,$2,$3,$4) RETURNING id::text`,
						tenantID, chatID, text, when).Scan(&id)
				}
				return tx.QueryRow(ctx,
					`INSERT INTO agent_reminders (tenant_id, bot_id, chat_id, text, remind_at) VALUES ($1,$2::uuid,$3,$4,$5) RETURNING id::text`,
					tenantID, botID, chatID, text, when).Scan(&id)
			})
			if err != nil {
				return "error: " + err.Error()
			}
			return fmt.Sprintf("reminder set for %s (id %s): %s", p.RemindAt, id, text)
		},
	}
}
