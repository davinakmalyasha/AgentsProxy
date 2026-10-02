// Package mcp exposes the proxy's ToolSet as an MCP server for new clients.
// Go-only, thin adapter — single shared LLM_API_KEY, central store.
// New vertical = just setup mcp/tool, no Python needed.
//
// Transport: Streamable-HTTP style JSON-RPC (initialize →
// notifications/initialized → tools/list → tools/call), compatible
// with spec clients (e.g. the LangChain mcp_bridge HTTP transport).
//
// Auth (credential-derived, never params):
//   - Authorization: Bearer <ADMIN_API_KEY> → role "admin" (cross-tenant
//     admin_* tools only, plus business tools with explicit tenant_id).
//   - X-Session-Token (staff → own tenant + role).
//   - X-Client-Token (client → own tenant, role forced to "client").
package mcp

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/authapi"
	"github.com/davinakmalyasha/agents-proxy/internal/cache"
)

// Server is a minimal MCP-compatible HTTP handler (JSON-RPC 2.0).
type Server struct {
	pool      *pgxpool.Pool
	adminPool *pgxpool.Pool // cross-tenant reads for admin tools (bypasses RLS)
	cache     *cache.Client
	auth      *authapi.Auth
	adminKey  string
}

func New(pool *pgxpool.Pool, cacheCli *cache.Client, auth *authapi.Auth, adminPool *pgxpool.Pool, adminKey string) *Server {
	return &Server{pool: pool, cache: cacheCli, auth: auth, adminPool: adminPool, adminKey: adminKey}
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *mcpError `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		// Simple discovery (legacy): list tool names for the caller's role.
		tenantID, role, ok := s.resolve(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, mcpResponse{JSONRPC: "2.0", Error: &mcpError{Code: -32001, Message: "unauthorized: send Bearer ADMIN_API_KEY, X-Session-Token or X-Client-Token"}})
			return
		}
		_ = tenantID
		writeJSON(w, http.StatusOK, map[string]any{"tools": s.toolDefs(role), "role": role})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, mcpResponse{JSONRPC: "2.0", Error: &mcpError{Code: -32601, Message: "method not allowed"}})
		return
	}
	var req mcpRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, mcpResponse{JSONRPC: "2.0", Error: &mcpError{Code: -32700, Message: "parse error"}})
		return
	}
	switch req.Method {
	case "initialize":
		writeJSON(w, http.StatusOK, mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "agents-proxy", "version": "1.0.0"},
		}})
	case "notifications/initialized":
		// Notification: no response body per spec (empty result is tolerated).
		writeJSON(w, http.StatusOK, mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}})
	case "tools/list":
		tenantID, role, ok := s.resolve(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, mcpResponse{JSONRPC: "2.0", ID: req.ID, Error: &mcpError{Code: -32001, Message: "unauthorized"}})
			return
		}
		_ = tenantID
		writeJSON(w, http.StatusOK, mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": s.toolDefs(role)}})
	case "tools/call":
		s.handleCall(w, r, req)
	default:
		writeJSON(w, http.StatusNotFound, mcpResponse{JSONRPC: "2.0", ID: req.ID, Error: &mcpError{Code: -32601, Message: "method not found"}})
	}
}

// toolDef is one MCP-spec tool description.
type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func objSchema(required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "required": required}
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// toolDefs returns MCP-spec definitions for a role.
func (s *Server) toolDefs(role string) []toolDef {
	if role == "admin" {
		return adminToolDefs()
	}
	ts := agentstools.NewToolSetFor(s.pool, role)
	out := []toolDef{}
	for _, d := range ts.Definitions() {
		// Definitions() yields "- name: description".
		name, desc := splitDef(d)
		out = append(out, toolDef{Name: name, Description: desc, InputSchema: objSchema()})
	}
	return out
}

func splitDef(d string) (string, string) {
	d = strings.TrimPrefix(strings.TrimSpace(d), "- ")
	if i := strings.Index(d, ":"); i > 0 {
		return strings.TrimSpace(d[:i]), strings.TrimSpace(d[i+1:])
	}
	return d, ""
}

func (s *Server) handleCall(w http.ResponseWriter, r *http.Request, req mcpRequest) {
	tenantID, role, ok := s.resolve(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, mcpResponse{JSONRPC: "2.0", ID: req.ID, Error: &mcpError{Code: -32001, Message: "unauthorized"}})
		return
	}
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeJSON(w, http.StatusBadRequest, mcpResponse{JSONRPC: "2.0", ID: req.ID, Error: &mcpError{Code: -32602, Message: "invalid params"}})
		return
	}
	// Admin tools (cross-tenant, trusted credential only).
	if strings.HasPrefix(p.Name, "admin_") {
		if role != "admin" {
			writeJSON(w, http.StatusOK, mcpResponse{JSONRPC: "2.0", ID: req.ID, Error: &mcpError{Code: -32602, Message: "admin only"}})
			return
		}
		result := s.runAdminTool(r.Context(), p.Name, p.Arguments)
		writeJSON(w, http.StatusOK, mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"content": []map[string]string{{"type": "text", "text": result}}}})
		return
	}
	// Business tools: admin callers pass explicit tenant_id (operator
	// support); everyone else uses their credential's tenant.
	if role == "admin" {
		var a struct {
			TenantID string `json:"tenant_id"`
		}
		_ = json.Unmarshal(p.Arguments, &a)
		if a.TenantID == "" {
			writeJSON(w, http.StatusOK, mcpResponse{JSONRPC: "2.0", ID: req.ID, Error: &mcpError{Code: -32602, Message: "admin calls need explicit tenant_id argument"}})
			return
		}
		tenantID = a.TenantID
	}
	ts := agentstools.NewToolSetFor(s.pool, mapRole(role))
	tool, ok := ts.Get(p.Name)
	if !ok {
		writeJSON(w, http.StatusOK, mcpResponse{JSONRPC: "2.0", ID: req.ID, Error: &mcpError{Code: -32602, Message: "unknown tool or not allowed for role " + role}})
		return
	}
	// Same RLS path as REST — tenant isolation inherited
	result := tool.Run(r.Context(), tenantID, p.Arguments)
	writeJSON(w, http.StatusOK, mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"content": []map[string]string{{"type": "text", "text": result}}}})
}

// mapRole maps the MCP identity to a ToolSet role: admin operates with
// the owner toolset (tenant still scoped per call).
func mapRole(role string) string {
	if role == "admin" {
		return "owner"
	}
	return role
}

// resolve maps a request to (tenantID, role) from credentials only.
// Bearer ADMIN_API_KEY wins (admin, no tenant); then session (staff);
// else client token (role forced to client).
func (s *Server) resolve(r *http.Request) (tenantID, role string, ok bool) {
	if s.adminKey != "" {
		if auth, token, found := strings.Cut(r.Header.Get("Authorization"), " "); found && strings.EqualFold(auth, "Bearer") && subtleEqual(token, s.adminKey) {
			return "", "admin", true
		}
	}
	if tok := strings.TrimSpace(r.Header.Get("X-Session-Token")); tok != "" && s.auth != nil {
		if u, err := s.auth.UserByToken(r.Context(), tok); err == nil {
			role = u.Role
			if role != "owner" && role != "pegawai" {
				role = "pegawai"
			}
			return u.TenantID, role, true
		}
	}
	if tok := strings.TrimSpace(r.Header.Get("X-Client-Token")); tok != "" && s.cache != nil {
		if cm, err := s.cache.GetByToken(r.Context(), tok); err == nil && cm.Status == "active" {
			return cm.TenantID, "client", true
		}
	}
	return "", "", false
}

// subtleEqual compares secrets in constant time.
func subtleEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
