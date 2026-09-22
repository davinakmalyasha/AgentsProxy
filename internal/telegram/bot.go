// Package telegram is the Telegram channel for the agentic layer.
//
// It wraps the Telegram Bot API (getUpdates polling for dev, webhook
// for prod) and resolves incoming chat_id → client via agent_bindings.
// The bot is feature-gated: a client whose `features` lacks "telegram"
// gets no reply (the binding exists but the feature is off).
//
// DONE: Phase 5 (telegram agentic) / STANDARDS A, D, F
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// apiBase is the Telegram Bot API base URL. A var so tests can point
// at a fake server.
var apiBase = "https://api.telegram.org/bot%s"

// SetAPIBase overrides the API base URL (test helper; not for
// production use).
func SetAPIBase(base string) { apiBase = base }

// Update is a Telegram getUpdates result (the subset we need).
type Update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID int64 `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
}

// Bot is a minimal Telegram Bot API client.
type Bot struct {
	Token  string
	HTTP   *http.Client
	Offset int64 // last processed update id
}

// New returns a Bot with a sane HTTP client.
func New(token string) *Bot {
	return &Bot{
		Token: token,
		HTTP:  &http.Client{Timeout: 30 * time.Second},
	}
}

// Send posts a text message to a chat. Returns *RateError on Telegram
// 429 (caller should honor RetryAfter); other non-2xx are plain errors.
func (b *Bot) Send(ctx context.Context, chatID int64, text string) error {
	payload, _ := json.Marshal(map[string]any{
		"chat_id": chatID,
		"text":    text,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf(apiBase, b.Token)+"/sendMessage", strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return asTelegramError(resp)
	}
	return nil
}

// RateError is a Telegram 429 with the requested backoff.
type RateError struct {
	RetryAfter int
}

func (e *RateError) Error() string {
	return fmt.Sprintf("telegram rate limited, retry after %ds", e.RetryAfter)
}

// asTelegramError maps non-2xx Bot API responses: 429 → *RateError
// (parameters.retry_after), else a plain status error.
func asTelegramError(resp *http.Response) error {
	var body struct {
		Description string `json:"description"`
		Parameters  *struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode == http.StatusTooManyRequests {
		after := 5
		if body.Parameters != nil && body.Parameters.RetryAfter > 0 {
			after = body.Parameters.RetryAfter
		}
		return &RateError{RetryAfter: after}
	}
	if body.Description != "" {
		return fmt.Errorf("telegram send: status %d: %s", resp.StatusCode, body.Description)
	}
	return fmt.Errorf("telegram send: status %d", resp.StatusCode)
}

// BotInfo is the subset of getMe we store (username for logs/links).
type BotInfo struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// GetMe validates a token and returns the bot identity. Used when an
// owner registers a bot (proves the pasted token works).
func (b *Bot) GetMe(ctx context.Context) (BotInfo, error) {
	var out BotInfo
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf(apiBase, b.Token)+"/getMe", nil)
	if err != nil {
		return out, err
	}
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, asTelegramError(resp)
	}
	var wrap struct {
		OK     bool    `json:"ok"`
		Result BotInfo `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return out, err
	}
	if !wrap.OK {
		return out, errors.New("telegram getMe: not ok")
	}
	return wrap.Result, nil
}

// SetWebhook points Telegram at our hook URL with a per-bot secret.
// Telegram includes the secret in X-Telegram-Bot-Api-Secret-Token.
func (b *Bot) SetWebhook(ctx context.Context, url, secret string) error {
	payload, _ := json.Marshal(map[string]any{
		"url":                  url,
		"secret_token":         secret,
		"allowed_updates":      []string{"message"},
		"drop_pending_updates": true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf(apiBase, b.Token)+"/setWebhook", strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return asTelegramError(resp)
	}
	return nil
}

// DeleteWebhook removes the webhook (bot falls back to polling).
func (b *Bot) DeleteWebhook(ctx context.Context) error {
	payload, _ := json.Marshal(map[string]any{"drop_pending_updates": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf(apiBase, b.Token)+"/deleteWebhook", strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return asTelegramError(resp)
	}
	return nil
}

// SendDocument posts a file (e.g. chat-generated .xlsx) with an optional
// caption. Returns an error on non-2xx.
func (b *Bot) SendDocument(ctx context.Context, chatID int64, filename string, data []byte, caption string) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("chat_id", fmt.Sprintf("%d", chatID)); err != nil {
		return err
	}
	if caption != "" {
		if err := w.WriteField("caption", caption); err != nil {
			return err
		}
	}
	part, err := w.CreateFormFile("document", filename)
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, bytes.NewReader(data)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf(apiBase, b.Token)+"/sendDocument", &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return asTelegramError(resp)
	}
	return nil
}

// GetUpdates long-polls for new updates. Returns updates newer than
// the bot's offset. timeoutSeconds uses long-polling (e.g. 30).
func (b *Bot) GetUpdates(ctx context.Context, timeoutSeconds int) ([]Update, error) {
	payload, _ := json.Marshal(map[string]any{
		"offset":  b.Offset,
		"timeout": timeoutSeconds,
		"limit":   50,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf(apiBase, b.Token)+"/getUpdates", strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram getUpdates: status %d", resp.StatusCode)
	}
	var out struct {
		OK     bool     `json:"ok"`
		Result []Update `json:"result"`
		Error  *struct {
			Description string `json:"description"`
		} `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if !out.OK {
		if out.Error != nil {
			return nil, fmt.Errorf("telegram getUpdates: %s", out.Error.Description)
		}
		return nil, errors.New("telegram getUpdates: not ok")
	}
	if len(out.Result) > 0 {
		b.Offset = out.Result[len(out.Result)-1].UpdateID + 1
	}
	return out.Result, nil
}
