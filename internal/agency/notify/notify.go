// Package notify delivers outbound notifications. The default implementation
// logs to the structured logger; production swaps in Meta Cloud API / gateways
// without touching callers.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Service sends agency notifications.
type Service struct {
	logger         *slog.Logger
	agencyWhatsapp string
	resendKey      string
	resendFrom     string
	client         *http.Client
}

// New builds a notification service.
func New(logger *slog.Logger, agencyWhatsapp string) *Service {
	return &Service{logger: logger, agencyWhatsapp: agencyWhatsapp, client: &http.Client{Timeout: 15 * time.Second}}
}

// WithResend enables real email delivery via Resend. Without it SendEmail
// stays log-only (dev); callers needing delivery must fail closed.
func (s *Service) WithResend(apiKey, from string) *Service {
	s.resendKey = apiKey
	s.resendFrom = from
	return s
}

// ResendConfigured reports whether real email delivery is available.
func (s *Service) ResendConfigured() bool { return s.resendKey != "" && s.resendFrom != "" }

// SendWhatsApp accepts a WhatsApp message for delivery.
func (s *Service) SendWhatsApp(ctx context.Context, to, message string) error {
	// Placeholder transport: the message is recorded for the delivery worker.
	// A provider adapter (Meta Cloud API, Fonnte, Wablast) implements the same
	// call without changing the API contract.
	s.logger.InfoContext(ctx, "whatsapp outbox",
		"to", to,
		"agency_whatsapp", s.agencyWhatsapp,
		"message_len", len(message),
	)
	return nil
}

// SendEmail delivers via Resend, or logs when unconfigured (dev).
func (s *Service) SendEmail(ctx context.Context, to, subject, html string) error {
	if !s.ResendConfigured() {
		s.logger.InfoContext(ctx, "email outbox (unconfigured)", "to", to, "subject", subject)
		return nil
	}
	body, _ := json.Marshal(map[string]any{"from": s.resendFrom, "to": []string{to}, "subject": subject, "html": html})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.resend.com/emails", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.resendKey)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("resend: status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// Telegram posts agency alerts via the Bot API when configured.
type Telegram struct {
	logger *slog.Logger
	bot    string
	chatID string
	client *http.Client
}

// NewTelegram builds a Telegram poster. Empty bot/chat disables delivery.
func NewTelegram(logger *slog.Logger, bot, chatID string) *Telegram {
	return &Telegram{logger: logger, bot: bot, chatID: chatID, client: &http.Client{Timeout: 15 * time.Second}}
}

// Post sends a message, or logs when unconfigured.
func (t *Telegram) Post(ctx context.Context, message string) error {
	if t.bot == "" || t.chatID == "" {
		t.logger.InfoContext(ctx, "telegram outbox (unconfigured)", "message_len", len(message))
		return nil
	}
	body, _ := json.Marshal(map[string]any{"chat_id": t.chatID, "text": message})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.telegram.org/bot"+t.bot+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("telegram: status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
