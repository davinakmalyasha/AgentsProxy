package api

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/davinakmalyasha/agents-proxy/internal/adkrunner"
	"github.com/davinakmalyasha/agents-proxy/internal/authapi"
	"github.com/davinakmalyasha/agents-proxy/internal/features/clientctx"
	"github.com/davinakmalyasha/agents-proxy/internal/features/shared/records"
	"github.com/davinakmalyasha/agents-proxy/internal/tenanthealth"
	adksession "google.golang.org/adk/v2/session"
)

// AgentChat handles role-aware agent calls. Single shared LLM_API_KEY for all roles.
type AgentChat struct {
	Pool *pgxpool.Pool
	RDB  *redis.Client
	Auth *authapi.Auth
	// ADK is the agent loop. Boot is fatal without it.
	ADK *adkrunner.Runner
	// sem bounds concurrent agent turns. Caps and rate limits bound
	// throughput, not concurrency: without this, N concurrent chats
	// hold N pooled transactions + N provider calls against a pool
	// capped at 20. Nil means unbounded (tests).
	sem chan struct{}
}

// maxConcurrentAgentTurns caps simultaneous RunWithToolsRole calls.
const maxConcurrentAgentTurns = 8

// acquireTurn takes a turn slot or reports whether the server is full.
func (h *AgentChat) acquireTurn() (release func(), ok bool) {
	if h.sem == nil {
		return func() {}, true
	}
	select {
	case h.sem <- struct{}{}:
		return func() { <-h.sem }, true
	default:
		return nil, false
	}
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

// ClientChat is public â€” landing widget (client FAQ/availability only, no money).
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
	if utf8.RuneCountInString(msg) > maxMessageRunes {
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
		writeJSONError(w, http.StatusBadRequest, "tenant_id required")
		return
	}
	// Tenant arrives from user input: must be a real active client,
	// never an arbitrary string (history keys, RLS context, tool reads).
	known, terr := records.CheckTenantCached(r.Context(), h.RDB, h.Pool, tenant)
	if terr != nil {
		slog.Error("client chat tenant lookup failed", "tenant", tenant, "err", terr)
		writeJSONError(w, http.StatusServiceUnavailable, "tenant lookup unavailable")
		return
	}
	if !known {
		writeJSONError(w, http.StatusBadRequest, "unknown tenant")
		return
	}
	if !quarantineCheck(w, tenant) {
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
	//
	// Cutover complete: the ADK loop with the same pairing (anon tools,
	// client persona). Boot is fatal without the runner, so no fallback.
	release, ok := h.acquireTurn()
	if !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "server busy — coba lagi")
		return
	}
	defer release()
	reply, err := h.ADK.RunWithToolsRole(r.Context(), tenant, vertical, userName, "anon", "client", msg)
	if err != nil {
		if quarantineErr(w, tenant, err) {
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			writeJSONError(w, http.StatusGatewayTimeout, "agent timeout — coba lagi")
			return
		}
		slog.Error("client chat run failed", "tenant", tenant, "err", err)
		writeJSONError(w, http.StatusBadGateway, "provider error")
		return
	}
	writeJSON(w, http.StatusOK, agentChatResponse{Reply: reply, Role: "client"})
}

// quarantineCheck fails closed for fenced tenants: 503 with the distinct
// quarantine body, before any model spend. Returns false when the request
// must stop.
func quarantineCheck(w http.ResponseWriter, tenant string) bool {
	if q, reason := tenanthealth.Check(tenant); q {
		slog.Warn("quarantined tenant blocked from agent", "tenant", tenant, "reason", reason)
		writeJSONError(w, http.StatusServiceUnavailable, tenanthealth.ResponseBody)
		return false
	}
	return true
}

// quarantineErr maps a runner quarantine refusal to the same 503. All
// other runner errors stay 502 provider errors.
func quarantineErr(w http.ResponseWriter, tenant string, err error) bool {
	if errors.Is(err, tenanthealth.ErrQuarantined) {
		writeJSONError(w, http.StatusServiceUnavailable, tenanthealth.ResponseBody)
		return true
	}
	return false
}
// clientIP lives in clientip.go (trust-boundary aware shared helper).
// ClearHistory deletes the caller's client-bot memory (landing "Bersihkan
// riwayat"). Client-only by design: staff/owner memory (keyed by email) is
// important and has no clear path. Only ag: history keys are deleted â€”
// quota (rpd:) keys are never touched, so clearing can't reset rate limits.
// DELETE /v1/agent/history?tenant_id=centrum
func (h *AgentChat) ClearHistory(w http.ResponseWriter, r *http.Request) {
	tenant := strings.TrimSpace(r.URL.Query().Get("tenant_id"))
	if tenant == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_id required")
		return
	}
	// Same guard as Chat: the tenant prefixes the Redis history keys, so
	// an unvalidated value lets a caller enumerate or wipe another
	// tenant's memory. Check it before building any key.
	known, err := records.CheckTenantCached(r.Context(), h.RDB, h.Pool, tenant)
	if err != nil {
		slog.Error("clear history tenant lookup failed", "tenant", tenant, "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "tenant lookup unavailable")
		return
	}
	if !known {
		writeJSONError(w, http.StatusBadRequest, "unknown tenant")
		return
	}
	if !quarantineCheck(w, tenant) {
		return
	}
	hist := &adkrunner.History{RDB: h.RDB}
	if err := hist.ClearForRole(r.Context(), tenant, "client", "client:"+clientIP(r)); err != nil {
		writeJSONError(w, http.StatusBadGateway, "clear failed")
		return
	}
	// ADK sessions (Postgres) hold post-cutover conversations under the
	// same (tenant, role, user) partition; wipe those too. A missing
	// session is not a failure -- the user may never have talked to the
	// new loop.
	if h.ADK != nil {
		if err := h.ADK.ClearSession(r.Context(), tenant, "client:"+clientIP(r), "client"); err != nil &&
			!errors.Is(err, adksession.ErrNotFound) {
			writeJSONError(w, http.StatusBadGateway, "clear failed")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"cleared": "client"})
}

// Chat is for staff/owner via X-Session-Token (role determined, per-role caps 20/80/150)
// Mounted at POST /v1/agent/chat â€” does NOT go through withTenant middleware, so resolve here.
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
	if role != clientctx.RoleOwner && role != clientctx.RoleStaff {
		role = clientctx.RoleStaff
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
	if utf8.RuneCountInString(msg) > maxMessageRunes {
		writeJSONError(w, http.StatusBadRequest, "message too long")
		return
	}
	tenant := u.TenantID
	if !quarantineCheck(w, tenant) {
		return
	}
	vertical := req.Vertical
	if vertical == "" {
		vertical = "sports"
	}
	// Cutover complete: the ADK loop, same tier for tools and persona
	// (unlike ClientChat's anon/client split -- here the session token
	// pins the tenant, so the role's own tool set is safe).
	release, ok := h.acquireTurn()
	if !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "server busy — coba lagi")
		return
	}
	defer release()
	reply, err := h.ADK.RunWithRole(r.Context(), tenant, vertical, u.Email, role, msg)
	if err != nil {
		if quarantineErr(w, tenant, err) {
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			writeJSONError(w, http.StatusGatewayTimeout, "agent timeout — coba lagi")
			return
		}
		slog.Error("agent chat run failed", "tenant", tenant, "role", role, "err", err)
		writeJSONError(w, http.StatusBadGateway, "provider error")
		return
	}
	writeJSON(w, http.StatusOK, agentChatResponse{Reply: reply, Role: role})
}
