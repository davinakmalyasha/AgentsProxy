// DONE: PRD 6.1 (chat handler, quota, usage) / STANDARDS A, D, F
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/llm"
	"github.com/davinakmalyasha/agents-proxy/internal/middleware"
	"github.com/davinakmalyasha/agents-proxy/internal/quota"
	"github.com/davinakmalyasha/agents-proxy/internal/tenanthealth"
	"github.com/davinakmalyasha/agents-proxy/internal/usage"
)

type Chat struct {
	Pool  *pgxpool.Pool
	Cache *cache.Client
	LLM   *llm.Client
	RDB   *redis.Client
	// rateLimitPerMinute is the per-client request budget, applied once
	// the body token resolves. 0 disables it.
	rateLimitPerMinute int
}

// NewChat builds the chat handler with the per-client request budget.
func NewChat(pool *pgxpool.Pool, cache *cache.Client, l *llm.Client, rdb *redis.Client, perMinute int) *Chat {
	return &Chat{Pool: pool, Cache: cache, LLM: l, RDB: rdb, rateLimitPerMinute: perMinute}
}

type chatRequest struct {
	ClientToken string        `json:"client_token"`
	Messages    []llm.Message `json:"messages"`
	Model       string        `json:"model"`
}

type chatResponse struct {
	Reply string    `json:"reply"`
	Usage chatUsage `json:"usage"`
}

type chatUsage struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// maxChatBody is the hard cap on /v1/chat request bodies. The proxy's
// own agent loop feeds this endpoint too; generous but bounded.
const maxChatBody = 1 << 20 // 1 MiB

// maxChatMessages bounds history plus the current message so a client
// can't replay an unbounded transcript against the shared LLM key.
const maxChatMessages = 60

// maxMessageRunes bounds a single message so one huge paste can't
// dominate a request or blow token accounting.
const maxMessageRunes = 50_000

// sanitizeModel maps a caller-supplied model onto the proxy's single
// shared-key allowlist. Unknown names fall back to the default so a
// stale/malicious model string can't trigger a cost-miscount or a
// different provider contract.
func sanitizeModel(requested, def string) string {
	if requested == "" {
		return def
	}
	switch strings.ToLower(requested) {
	case "deepseek-chat", "deepseek", "mimo", "command-code":
		// Canonical lowercase — the provider receives exactly the
		// allowlisted name, never caller-controlled casing.
		return strings.ToLower(requested)
	}
	return def
}

func (h *Chat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r, req, clientModel, model, ok := h.resolveAuth(w, r)
	if !ok {
		return
	}

	llmCtx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	reply, inTok, outTok, lerr := h.LLM.Chat(llmCtx, req.Messages, model)
	if lerr != nil {
		writeJSONError(w, http.StatusBadGateway, "provider error")
		return
	}

	// Skip zero-token rows: cost is $0, quota counts tokens (unchanged),
	// and the write would cost 1 DB RPC + 1 Lua to record $0. Request
	// volume is still observed via SLO/logging, not the usage ledger.
	if inTok != 0 || outTok != 0 {
		h.meter(r, clientModel.ID, model, inTok, outTok)
	}

	cost := usage.EstimateCost(inTok, outTok)
	writeJSON(w, http.StatusOK, chatResponse{
		Reply: reply,
		Usage: chatUsage{PromptTokens: inTok, CompletionTokens: outTok, CostUSD: cost},
	})
}

// streamDone is the terminal event of /v1/chat/stream. Clients that
// accumulate deltas ignore Reply; clients that only want the final
// answer read Reply + Usage here.
type streamDone struct {
	Done  bool      `json:"done"`
	Reply string    `json:"reply"`
	Usage chatUsage `json:"usage"`
}

// ServeStream is POST /v1/chat/stream: the same gate and metering as
// ServeHTTP, but the reply streams as server-sent events so the first
// token arrives at provider-TTFB instead of after the full completion.
// Every event is `data: <json>\n\n`:
//
//	{"delta":"..."}   content deltas, in order; concatenated they are the reply
//	{"done":true,"reply":"...","usage":{"prompt_tokens":N,"completion_tokens":M,"cost_usd":F}}
//	{"error":"provider error"}   only if the provider fails mid-stream
//
// Pre-stream failures (auth, validation, quota, rate limit) are plain
// JSON with the same status codes as /v1/chat, because headers are not
// sent until the gate passes. Only post-200 failures become events.
// Metering runs detached like ServeHTTP, including on partial streams
// (the provider bills what it generated), but is skipped when nothing
// was produced at all.
func (h *Chat) ServeStream(w http.ResponseWriter, r *http.Request) {
	r, req, clientModel, model, ok := h.resolveAuth(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		// No streaming support (never true on a real server, possible
		// in exotic tests): degrade to the JSON reply, same contract
		// as /v1/chat.
		llmCtx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		reply, inTok, outTok, lerr := h.LLM.Chat(llmCtx, req.Messages, model)
		if lerr != nil {
			writeJSONError(w, http.StatusBadGateway, "provider error")
			return
		}
		if inTok != 0 || outTok != 0 {
			h.meter(r, clientModel.ID, model, inTok, outTok)
		}
		cost := usage.EstimateCost(inTok, outTok)
		writeJSON(w, http.StatusOK, chatResponse{
			Reply: reply,
			Usage: chatUsage{PromptTokens: inTok, CompletionTokens: outTok, CostUSD: cost},
		})
		return
	}
	// Headers first, before the provider is even dialed: intermediaries
	// must see an event stream, not a stalled JSON body, so nothing
	// buffers waiting for the full completion.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	writeEvent := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	llmCtx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	reply, inTok, outTok, lerr := h.LLM.Stream(llmCtx, req.Messages, model, func(delta string) error {
		return writeEvent(map[string]string{"delta": delta})
	})
	// Same zero-token skip as ServeHTTP: a $0 row costs a DB RPC + Lua
	// and leaves quota unchanged. Partial streams with real tokens still
	// meter (provider bills what was generated).
	if inTok != 0 || outTok != 0 {
		h.meter(r, clientModel.ID, model, inTok, outTok)
	}
	if lerr != nil {
		_ = writeEvent(map[string]string{"error": "provider error"})
		return
	}
	cost := usage.EstimateCost(inTok, outTok)
	_ = writeEvent(streamDone{
		Done:  true,
		Reply: reply,
		Usage: chatUsage{PromptTokens: inTok, CompletionTokens: outTok, CostUSD: cost},
	})
}

// meter records usage detached from client cancellation. It must never
// break the reply the user already paid latency for — but it must not
// be lost either. A client that disconnects after the model call still
// gets metered. Warn, not Error: Error records auto-forward to Sentry
// with a stack trace, and a metering blip must not page.
func (h *Chat) meter(r *http.Request, clientID, model string, inTok, outTok int) {
	mctx, mcancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer mcancel()
	if err := usage.Record(mctx, h.Pool, h.RDB, clientID, model, "chat", inTok, outTok); err != nil {
		slog.Warn("usage record failed", "client_id", clientID, "err", err)
	}
}

// resolveAuth runs the shared pre-LLM gate for /v1/chat and
// /v1/chat/stream: body validation, token auth, quarantine, the
// per-client rate limit, and quota. It writes the error response and
// returns ok=false on any failure; callers must stop. On success it
// returns the updated request (client ID in context), the validated
// body, the client model, and the sanitized model.
func (h *Chat) resolveAuth(w http.ResponseWriter, r *http.Request) (*http.Request, *chatRequest, *cache.ClientModel, string, bool) {
	fail := func(status int, msg string) (*http.Request, *chatRequest, *cache.ClientModel, string, bool) {
		writeJSONError(w, status, msg)
		return nil, nil, nil, "", false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxChatBody)
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fail(http.StatusBadRequest, "invalid json")
	}
	if req.ClientToken == "" {
		return fail(http.StatusUnauthorized, "missing client_token")
	}
	if len(req.Messages) == 0 {
		return fail(http.StatusBadRequest, "messages required")
	}
	if len(req.Messages) > maxChatMessages {
		return fail(http.StatusBadRequest, "too many messages")
	}
	for _, m := range req.Messages {
		if m.Content == "" {
			return fail(http.StatusBadRequest, "empty message content")
		}
		if m.Role != "system" && m.Role != "user" && m.Role != "assistant" {
			return fail(http.StatusBadRequest, "invalid message role")
		}
		if utf8.RuneCountInString(m.Content) > maxMessageRunes {
			return fail(http.StatusBadRequest, "message too long")
		}
	}
	// Force a single model for this shared-key endpoint. req.Model only
	// selects among the allowlist; anything else uses the default.
	req.Model = sanitizeModel(req.Model, h.LLM.DefaultModel)

	clientModel, err := h.Cache.GetByToken(r.Context(), req.ClientToken)
	if err != nil {
		if errors.Is(err, cache.ErrInvalidToken) {
			return fail(http.StatusUnauthorized, "invalid token")
		}
		return fail(http.StatusInternalServerError, "internal")
	}
	if clientModel.Status != "active" {
		return fail(http.StatusForbidden, "client "+clientModel.Status)
	}
	if q, reason := tenanthealth.Check(clientModel.TenantID); q {
		slog.Warn("quarantined tenant blocked from chat", "tenant", clientModel.TenantID, "reason", reason)
		return fail(http.StatusServiceUnavailable, tenanthealth.ResponseBody)
	}

	// Make client_id visible to logging + rate-limit middleware.
	r = r.WithContext(middleware.SetClientID(r.Context(), clientModel.ID))

	// Per-account request budget. The outer /v1 limiter runs before the
	// token in the body is resolved, so it can only key by IP; this is
	// where the client id finally exists. The monthly quota below is
	// the spend ceiling, but a rate limit keeps a single noisy caller
	// from monopolising the shared LLM connection pool.
	if ok, retryAfter := middleware.AllowClient(r.Context(), h.RDB, h.rateLimitPerMinute, "chat", clientModel.ID); !ok {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "rate limit exceeded"})
		return nil, nil, nil, "", false
	}

	if _, _, qerr := quota.Check(r.Context(), h.Pool, h.RDB, clientModel.ID, clientModel.MonthlyQuotaTokens); qerr != nil {
		if quota.IsExceeded(qerr) {
			resetAt := qeResetAt(qerr)
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(time.Until(resetAt).Seconds())+1))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":   "quota exceeded",
				"resetAt": resetAt.UTC().Format(time.RFC3339),
			})
			return nil, nil, nil, "", false
		}
		return fail(http.StatusInternalServerError, "internal")
	}

	model := sanitizeModel(req.Model, h.LLM.DefaultModel)
	return r, &req, clientModel, model, true
}
