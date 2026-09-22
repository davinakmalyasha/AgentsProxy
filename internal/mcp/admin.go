// Cross-tenant admin tools for the super-operator (T-agents-admin).
//
// Reachable ONLY with Authorization: Bearer <ADMIN_API_KEY> (the human
// UI keeps that key server-side; workspace agents hold it in process
// env). Reads use the admin pool (bypasses tenant RLS, like the
// reminder scheduler). Tokens are never selected or returned.
//
// DONE: T-agents-admin
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func schemaProps(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func adminToolDefs() []toolDef {
	s := strProp
	return []toolDef{
		{Name: "admin_proxy_health", Description: "Proxy-wide health snapshot: tenants, bots, reminders, sessions.",
			InputSchema: schemaProps(map[string]any{})},
		{Name: "admin_clients_list", Description: "List all client tenants (tenant_id, name, status, vertical).",
			InputSchema: schemaProps(map[string]any{})},
		{Name: "admin_client_usage", Description: "Usage snapshot for one tenant: users, bookings, bots, reminders.",
			InputSchema: schemaProps(map[string]any{"tenant_id": s("Client tenant_id")}, "tenant_id")},
		{Name: "admin_bots_list", Description: "List Telegram bots, optionally filtered by tenant. Tokens never returned.",
			InputSchema: schemaProps(map[string]any{"tenant_id": s("Optional tenant filter")})},
		{Name: "admin_bot_disable", Description: "Disable a bot now (stops replies; webhook 404s). Reversible via bot PATCH.",
			InputSchema: schemaProps(map[string]any{"bot_id": s("Bot UUID")}, "bot_id")},
		{Name: "admin_bot_allowlist", Description: "Replace a bot's owner/pegawai chat allowlist (empty array refused for those roles).",
			InputSchema: schemaProps(map[string]any{
				"bot_id":   s("Bot UUID"),
				"chat_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
			}, "bot_id", "chat_ids")},
		{Name: "admin_reminders_dead", Description: "Recently dead-lettered reminders (undeliverable).",
			InputSchema: schemaProps(map[string]any{
				"tenant_id": s("Optional tenant filter"),
				"limit":     s("Max rows (default 20, max 100)"),
			})},
	}
}

func (s *Server) runAdminTool(ctx context.Context, name string, raw json.RawMessage) string {
	if s.adminPool == nil {
		return "error: admin store unavailable"
	}
	var p struct {
		TenantID string  `json:"tenant_id"`
		BotID    string  `json:"bot_id"`
		ChatIDs  []int64 `json:"chat_ids"`
		Limit    int     `json:"limit"`
	}
	_ = json.Unmarshal(raw, &p)
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	switch name {
	case "admin_proxy_health":
		return s.adminHealth(cctx)
	case "admin_clients_list":
		return s.adminClientsList(cctx)
	case "admin_client_usage":
		if p.TenantID == "" {
			return "error: tenant_id required"
		}
		return s.adminClientUsage(cctx, p.TenantID)
	case "admin_bots_list":
		return s.adminBotsList(cctx, p.TenantID)
	case "admin_bot_disable":
		if p.BotID == "" {
			return "error: bot_id required"
		}
		return s.adminBotDisable(cctx, p.BotID)
	case "admin_bot_allowlist":
		if p.BotID == "" {
			return "error: bot_id required"
		}
		return s.adminBotAllowlist(cctx, p.BotID, p.ChatIDs)
	case "admin_reminders_dead":
		return s.adminRemindersDead(cctx, p.TenantID, p.Limit)
	default:
		return "error: unknown admin tool"
	}
}

func (s *Server) q1(ctx context.Context, sql string, args ...any) string {
	var v string
	if err := s.adminPool.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
		return "?"
	}
	return v
}

func (s *Server) adminHealth(ctx context.Context) string {
	return fmt.Sprintf(
		"tenants %s | bots %s active | reminders pending %s, dead %s | staff users %s | sessions %s",
		s.q1(ctx, `SELECT count(*)::text FROM clients`),
		s.q1(ctx, `SELECT count(*)::text FROM telegram_bots WHERE status='active'`),
		s.q1(ctx, `SELECT count(*)::text FROM agent_reminders WHERE status='pending'`),
		s.q1(ctx, `SELECT count(*)::text FROM agent_reminders WHERE status='dead'`),
		s.q1(ctx, `SELECT count(*)::text FROM sports_users WHERE is_active`),
		s.q1(ctx, `SELECT count(*)::text FROM sports_sessions WHERE expires_at > now()`),
	)
}

func (s *Server) adminClientsList(ctx context.Context) string {
	rows, err := s.adminPool.Query(ctx,
		`SELECT tenant_id, name, status, vertical FROM clients ORDER BY tenant_id LIMIT 200`)
	if err != nil {
		return "error: " + err.Error()
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t, n, st, v string
		if err := rows.Scan(&t, &n, &st, &v); err != nil {
			return "error: " + err.Error()
		}
		out = append(out, fmt.Sprintf("%s (%s) [%s/%s]", t, n, st, v))
	}
	if err := rows.Err(); err != nil {
		return "error: " + err.Error()
	}
	if len(out) == 0 {
		return "no clients"
	}
	return strings.Join(out, "\n")
}

func (s *Server) adminClientUsage(ctx context.Context, tenant string) string {
	return fmt.Sprintf(
		"%s: users %s, bookings %s, bots %s (active %s), reminders pending %s/dead %s",
		tenant,
		s.q1(ctx, `SELECT count(*)::text FROM sports_users WHERE tenant_id=$1`, tenant),
		s.q1(ctx, `SELECT count(*)::text FROM sports_bookings WHERE tenant_id=$1`, tenant),
		s.q1(ctx, `SELECT count(*)::text FROM telegram_bots WHERE tenant_id=$1`, tenant),
		s.q1(ctx, `SELECT count(*)::text FROM telegram_bots WHERE tenant_id=$1 AND status='active'`, tenant),
		s.q1(ctx, `SELECT count(*)::text FROM agent_reminders WHERE tenant_id=$1 AND status='pending'`, tenant),
		s.q1(ctx, `SELECT count(*)::text FROM agent_reminders WHERE tenant_id=$1 AND status='dead'`, tenant),
	)
}

func (s *Server) adminBotsList(ctx context.Context, tenant string) string {
	sql := `SELECT id::text, tenant_id, name, role, status,
	              coalesce(array_length(allow_chat_ids,1),0),
	              last_update_id
	         FROM telegram_bots`
	var rows pgx.Rows
	var err error
	if tenant != "" {
		rows, err = s.adminPool.Query(ctx, sql+` WHERE tenant_id=$1 ORDER BY name`, tenant)
	} else {
		rows, err = s.adminPool.Query(ctx, sql+` ORDER BY tenant_id, name LIMIT 200`)
	}
	if err != nil {
		return "error: " + err.Error()
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, t, n, r, st string
		var allows, lastUpd int64
		if err := rows.Scan(&id, &t, &n, &r, &st, &allows, &lastUpd); err != nil {
			return "error: " + err.Error()
		}
		out = append(out, fmt.Sprintf("%s %s/%s [%s/%s] allows:%d upd:%d", id[:8], t, n, r, st, allows, lastUpd))
	}
	if err := rows.Err(); err != nil {
		return "error: " + err.Error()
	}
	if len(out) == 0 {
		return "no bots"
	}
	return strings.Join(out, "\n")
}

func (s *Server) adminBotDisable(ctx context.Context, botID string) string {
	tag, err := s.adminPool.Exec(ctx,
		`UPDATE telegram_bots SET status='disabled', updated_at=now() WHERE id=$1::uuid`, botID)
	if err != nil {
		return "error: " + shortErr(err)
	}
	if tag.RowsAffected() == 0 {
		return "error: bot not found"
	}
	return "disabled " + botID
}

func (s *Server) adminBotAllowlist(ctx context.Context, botID string, chats []int64) string {
	if chats == nil {
		return "error: chat_ids required"
	}
	var role string
	if err := s.adminPool.QueryRow(ctx,
		`SELECT role FROM telegram_bots WHERE id=$1::uuid`, botID).Scan(&role); err != nil {
		return "error: bot not found"
	}
	if (role == "owner" || role == "pegawai") && len(chats) == 0 {
		return "error: owner/pegawai bots need a non-empty allowlist"
	}
	tag, err := s.adminPool.Exec(ctx,
		`UPDATE telegram_bots SET allow_chat_ids=$2, updated_at=now() WHERE id=$1::uuid`, botID, chats)
	if err != nil {
		return "error: " + shortErr(err)
	}
	if tag.RowsAffected() == 0 {
		return "error: bot not found"
	}
	return fmt.Sprintf("allowlist set (%d chats) on %s", len(chats), botID)
}

func (s *Server) adminRemindersDead(ctx context.Context, tenant string, limit int) string {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	sql := `SELECT id::text, tenant_id, chat_id, left(text, 80), attempts, remind_at::text
	          FROM agent_reminders WHERE status='dead'`
	var rows pgx.Rows
	var err error
	if tenant != "" {
		rows, err = s.adminPool.Query(ctx, sql+` AND tenant_id=$1 ORDER BY remind_at DESC LIMIT $2`, tenant, limit)
	} else {
		rows, err = s.adminPool.Query(ctx, sql+` ORDER BY remind_at DESC LIMIT $1`, limit)
	}
	if err != nil {
		return "error: " + err.Error()
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, t, chat, text, at string
		var attempts int
		if err := rows.Scan(&id, &t, &chat, &text, &attempts, &at); err != nil {
			return "error: " + err.Error()
		}
		out = append(out, fmt.Sprintf("%s %s chat=%s tries=%d @%s: %s", id[:8], t, chat, attempts, at, text))
	}
	if err := rows.Err(); err != nil {
		return "error: " + err.Error()
	}
	if len(out) == 0 {
		return "no dead reminders"
	}
	return strings.Join(out, "\n")
}

func shortErr(err error) string {
	s := err.Error()
	if strings.Contains(s, "invalid input syntax") {
		return "invalid id"
	}
	if len(s) > 140 {
		s = s[:140]
	}
	return s
}
