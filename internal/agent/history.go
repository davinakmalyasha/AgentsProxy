// Conversation history per tenant (Redis-backed).
//
// DONE: Phase 5 (telegram agentic) / STANDARDS A, E
package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/llm"
)

const (
	histKeyPrefix = "ag:"
	histTTL       = 24 * time.Hour
	maxTurns      = 10 // last N (user,assistant) pairs
)

// History stores recent turns per tenant+role+user in Redis (good-as-possible scale).
// Single shared Redis, central store for all bots.
type History struct {
	RDB *redis.Client
}

func histKey(tenantID, role, userID string) string {
	if role == "" {
		role = "owner"
	}
	if userID == "" {
		userID = "anon"
	}
	// per-role isolation: ag:{tenant}:{role}:{userId}
	return histKeyPrefix + tenantID + ":" + role + ":" + userID
}

// Load returns the last maxTurns messages for the tenant (legacy, for owner).
func (h *History) Load(tenantID string) []llm.Message {
	return h.LoadForRole(tenantID, "owner", "anon")
}

// LoadForRole returns history for tenant+role+user (good-as-possible isolation).
func (h *History) LoadForRole(tenantID, role, userID string) []llm.Message {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := h.RDB.LRange(ctx, histKey(tenantID, role, userID), 0, -1).Result()
	if err != nil {
		return nil
	}
	var out []llm.Message
	for _, s := range raw {
		var m llm.Message
		if json.Unmarshal([]byte(s), &m) == nil {
			out = append(out, m)
		}
	}
	if len(out) > maxTurns*2 {
		out = out[len(out)-maxTurns*2:]
	}
	return out
}

// ClearForRole deletes the stored turns for tenant+role+user.
// Used by the client "Bersihkan riwayat" flow. Quota (rpd:) keys are
// separate and never touched here, so clearing can't reset rate limits.
func (h *History) ClearForRole(tenantID, role, userID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return h.RDB.Del(ctx, histKey(tenantID, role, userID)).Err()
}

// Append stores one (user, assistant) exchange (legacy).
func (h *History) Append(tenantID, userMsg, assistantMsg string) {
	h.AppendForRole(tenantID, "owner", "anon", userMsg, assistantMsg)
}

// AppendForRole stores per tenant+role+user.
func (h *History) AppendForRole(tenantID, role, userID, userMsg, assistantMsg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	key := histKey(tenantID, role, userID)
	pairs := []llm.Message{
		{Role: "user", Content: userMsg},
		{Role: "assistant", Content: assistantMsg},
	}
	for _, m := range pairs {
		b, _ := json.Marshal(m)
		h.RDB.RPush(ctx, key, string(b))
	}
	h.RDB.LTrim(ctx, key, -maxTurns*2, -1)
	h.RDB.Expire(ctx, key, histTTL)
}
