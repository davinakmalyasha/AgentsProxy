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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/llm"
	"github.com/davinakmalyasha/agents-proxy/internal/middleware"
	"github.com/davinakmalyasha/agents-proxy/internal/quota"
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
	r.Body = http.MaxBytesReader(w, r.Body, maxChatBody)
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.ClientToken == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing client_token")
		return
	}
	if len(req.Messages) == 0 {
		writeJSONError(w, http.StatusBadRequest, "messages required")
		return
	}
	if len(req.Messages) > maxChatMessages {
		writeJSONError(w, http.StatusBadRequest, "too many messages")
		return
	}
	for _, m := range req.Messages {
		if m.Content == "" {
			writeJSONError(w, http.StatusBadRequest, "empty message content")
			return
		}
		if m.Role != "system" && m.Role != "user" && m.Role != "assistant" {
			writeJSONError(w, http.StatusBadRequest, "invalid message role")
			return
		}
		if len([]rune(m.Content)) > maxMessageRunes {
			writeJSONError(w, http.StatusBadRequest, "message too long")
			return
		}
	}
	// Force a single model for this shared-key endpoint. req.Model only
	// selects among the allowlist; anything else uses the default.
	req.Model = sanitizeModel(req.Model, h.LLM.DefaultModel)

	clientModel, err := h.Cache.GetByToken(r.Context(), req.ClientToken)
	if err != nil {
		if errors.Is(err, cache.ErrInvalidToken) {
			writeJSONError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	if clientModel.Status != "active" {
		writeJSONError(w, http.StatusForbidden, "client "+clientModel.Status)
		return
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
		return
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
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}

	llmCtx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	model := sanitizeModel(req.Model, h.LLM.DefaultModel)
	reply, inTok, outTok, lerr := h.LLM.Chat(llmCtx, req.Messages, model)
	if lerr != nil {
		writeJSONError(w, http.StatusBadGateway, "provider error")
		return
	}

	// Metering failure must page but never break the reply the user
	// already paid latency for.
	if err := usage.Record(r.Context(), h.Pool, h.RDB, clientModel.ID, model, "chat", inTok, outTok); err != nil {
		slog.Error("usage record failed", "client_id", clientModel.ID, "err", err)
	}

	cost := usage.EstimateCost(inTok, outTok)
	writeJSON(w, http.StatusOK, chatResponse{
		Reply: reply,
		Usage: chatUsage{PromptTokens: inTok, CompletionTokens: outTok, CostUSD: cost},
	})
}
