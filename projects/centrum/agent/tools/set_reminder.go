// Package centrumbadminton implements Centrum Badminton's agent tools.
//
// One file per tool. Each file is self-contained: the Args shape, the
// description the model sees, and the Run function that executes it.
package centrumbadminton

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func init() { agentstools.Register("centrum", "set_reminder", setReminderTool) }

func setReminderTool(pool *pgxpool.Pool) agentstools.Tool {
	return agentstools.Tool{
		Name: "set_reminder", Mutating: true,
		Description: "Set a push-delivered reminder. The recipient is a staff " +
			"email matching a subscribed PWA device (see user list); reminders " +
			"to unsubscribed recipients wait in pending, never lost.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"what to be reminded about","maxLength":500},"remind_at":{"type":"string","description":"when, YYYY-MM-DD HH:MM WIB"},"recipient":{"type":"string","description":"staff email to notify"}},"required":["text","remind_at","recipient"]}`),
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			var p struct {
				Text      string `json:"text"`
				RemindAt  string `json:"remind_at"`
				Recipient string `json:"recipient"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return "error: text, remind_at and recipient required"
			}
			text := strings.TrimSpace(p.Text)
			if text == "" || len(text) > 500 {
				return "error: text required (max 500 chars)"
			}
			// Routability is enforced here, at creation: an unroutable
			// row must never be stored. user_ref is the staff email —
			// the same value POST /v1/push/subscriptions records.
			recipient := strings.ToLower(strings.TrimSpace(p.Recipient))
			if !strings.Contains(recipient, "@") || len(recipient) > 254 {
				return "error: recipient must be a staff email"
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
			err = records.WithTenantTx(ctx, pool, tenantID, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx,
					`INSERT INTO public.agent_reminders (tenant_id, text, remind_at, recipient_ref)
					 VALUES ($1, $2, $3, $4) RETURNING id::text`,
					tenantID, text, when, recipient).Scan(&id)
			})
			if err != nil {
				return "error: " + err.Error()
			}
			return fmt.Sprintf("reminder set for %s to %s (id %s): %s", p.RemindAt, recipient, id, text)
		},
	}
}
