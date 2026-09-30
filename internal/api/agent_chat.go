package api

import (
	"encoding/json"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"log/slog"
	"net/http"
	"strings"

	"github.com/davinakmalyasha/agents-proxy/internal/agent"
	"github.com/davinakmalyasha/agents-proxy/internal/authapi"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/davinakmalyasha/agents-proxy/internal/llm"
)

// AgentChat handles role-aware agent calls. Single shared LLM_API_KEY for all roles.
type AgentChat struct {
	Pool *pgxpool.Pool
	LLM  *llm.Client
	RDB  *redis.Client
	Auth *authapi.Auth
}

type agentChatRequest struct {
	Message  string `json:"message"`
	TenantID string `json:"tenant_id"`
	Vertical string `json:"vertical"`
}

type agentChatResponse struct {
	Reply string `json:"reply"`
	Role  string `json:"role"`
}

// clientIP resolves the caller IP the same way for chat + clear, so
// "Bersihkan riwayat" wipes exactly the key that ClientChat reads/writes.
// History is Redis-only and never client-supplied (prevents prompt injection).

// ClientChat is public — landing widget (client FAQ/availability only, no money).
// POST /v1/agent/client-chat?tenant_id=centrum  {message}
func (h *AgentChat) ClientChat(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxChatBody)
	var req agentChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		writeJSONError(w, http.StatusBadRequest, "message required")
		return
	}
	if len([]rune(msg)) > maxMessageRunes {
		writeJSONError(w, http.StatusBadRequest, "message too long")
		return
	}
	tenant := strings.TrimSpace(req.TenantID)
	if tenant == "" {
		// Trim here too: records.tenantFromQuery trims this parameter, and
		// an untrimmed " centrum " must not become a different tenant than
		// the same value in the body would.
		tenant = strings.TrimSpace(r.URL.Query().Get("tenant_id"))
	}
	if tenant == "" {
		tenant = "centrum"
	}
	// Tenant arrives from user input: must be a real active client,
	// never an arbitrary string (history keys, RLS context, tool reads).
	known, terr := records.CheckTenant(r.Context(), h.Pool, tenant)
	if terr != nil {
		slog.Error("client chat tenant lookup failed", "tenant", tenant, "err", terr)
		writeJSONError(w, http.StatusServiceUnavailable, "tenant lookup unavailable")
		return
	}
	if !known {
		writeJSONError(w, http.StatusBadRequest, "unknown tenant")
		return
	}
	vertical := req.Vertical
	if vertical == "" {
		vertical = "sports"
	}
	// Per-IP identity for anon client (not shared "Client" user)
	userName := "client:" + clientIP(r)
	// "anon" toolset, NOT "client": this endpoint carries no credential,
	// so the tools run against a tenant the CALLER chose. The anon tier
	// is deliberately read-free of tenant rows (pricing + availability
	// only); the client tier keeps list_bookings because there the
	// token pins the tenant via RLS. A public FAQ widget never needed
	// customer names, so nothing here can leak another tenant's data.
	ts := agent.NewToolSetFor(h.Pool, "anon")
	ag := agent.New(h.LLM, ts, &agent.History{RDB: h.RDB})
	reply, err := ag.RunWithRole(r.Context(), tenant, vertical, userName, "client", msg)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "provider error")
		return
	}
	writeJSON(w, http.StatusOK, agentChatResponse{Reply: reply, Role: "client"})
}

// clientIP lives in clientip.go (trust-boundary aware shared helper).
// ClearHistory deletes the caller's client-bot memory (landing "Bersihkan
// riwayat"). Client-only by design: staff/owner memory (keyed by email) is
// important and has no clear path. Only ag: history keys are deleted —
// quota (rpd:) keys are never touched, so clearing can't reset rate limits.
// DELETE /v1/agent/history?tenant_id=centrum
func (h *AgentChat) ClearHistory(w http.ResponseWriter, r *http.Request) {
	tenant := strings.TrimSpace(r.URL.Query().Get("tenant_id"))
	if tenant == "" {
		tenant = "centrum"
	}
	// Same guard as Chat: the tenant prefixes the Redis history keys, so
	// an unvalidated value lets a caller enumerate or wipe another
	// tenant's memory. Check it before building any key.
	known, err := records.CheckTenant(r.Context(), h.Pool, tenant)
	if err != nil {
		slog.Error("clear history tenant lookup failed", "tenant", tenant, "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "tenant lookup unavailable")
		return
	}
	if !known {
		writeJSONError(w, http.StatusBadRequest, "unknown tenant")
		return
	}
	hist := &agent.History{RDB: h.RDB}
	if err := hist.ClearForRole(tenant, "client", "client:"+clientIP(r)); err != nil {
		writeJSONError(w, http.StatusBadGateway, "clear failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"cleared": "client"})
}

// Chat is for staff/owner via X-Session-Token (role determined, per-role caps 20/80/150)
// Mounted at POST /v1/agent/chat — does NOT go through withTenant middleware, so resolve here.
func (h *AgentChat) Chat(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimSpace(r.Header.Get("X-Session-Token"))
	if tok == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing credentials")
		return
	}
	u, err := h.Auth.UserByToken(r.Context(), tok)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "invalid or expired session")
		return
	}
	role := u.Role
	if role != "owner" && role != "pegawai" {
		role = "pegawai"
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxChatBody)
	var req agentChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		writeJSONError(w, http.StatusBadRequest, "message required")
		return
	}
	if len([]rune(msg)) > maxMessageRunes {
		writeJSONError(w, http.StatusBadRequest, "message too long")
		return
	}
	tenant := u.TenantID
	vertical := req.Vertical
	if vertical == "" {
		vertical = "sports"
	}
	ts := agent.NewToolSetFor(h.Pool, role)
	ag := agent.New(h.LLM, ts, &agent.History{RDB: h.RDB})
	reply, err := ag.RunWithRole(r.Context(), tenant, vertical, u.Email, role, msg)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "provider error")
		return
	}
	writeJSON(w, http.StatusOK, agentChatResponse{Reply: reply, Role: role})
}
