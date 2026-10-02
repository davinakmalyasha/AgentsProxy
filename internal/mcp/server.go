// Package mcp exposes the proxy's ToolSet as an MCP server for new clients.
// Go-only, thin adapter — single shared LLM_API_KEY, central store.
// New vertical = just setup mcp/tool, no Python needed.
//
// Transport: the official mcp-go SDK in stateless Streamable-HTTP mode
// (initialize → notifications/initialized → tools/list → tools/call),
// compatible with spec clients (e.g. the LangChain mcp_bridge HTTP
// transport). Protocol versions negotiate: a client asking for
// 2025-06-18 gets 2025-06-18 back.
//
// Auth (credential-derived, never params) happens in ServeHTTP BEFORE
// the SDK sees the request, and the (tenant, role) travels in ctx, so
// the tool filter and every handler share one auth decision:
//   - Authorization: Bearer <ADMIN_API_KEY> → role "admin" (cross-tenant
//     admin_* tools, plus business tools with explicit tenant_id).
//   - X-Session-Token (staff → own tenant + role).
//   - X-Client-Token (client → own tenant, role forced to "client").
//
// Role isolation is the SDK's ToolFilter, which gates tools/call as well
// as tools/list (a filtered-out tool can be neither discovered nor
// invoked). Tool failures come back as standard isError results, not
// JSON-RPC errors, so any spec client understands them.
package mcp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	mcpsrv "github.com/mark3labs/mcp-go/server"

	"github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/authapi"
	"github.com/davinakmalyasha/agents-proxy/internal/cache"
)

// identity is the credential-derived caller for one request.
type identity struct {
	tenantID string
	role     string
}

// idKey carries the identity through the SDK, whose filter and handlers
// receive only ctx. Unexported struct: no collisions, no confusion with
// the agent packages' chat/bot keys.
type idKey struct{}

func withIdentity(ctx context.Context, id identity) context.Context {
	return context.WithValue(ctx, idKey{}, id)
}

func identityFromCtx(ctx context.Context) (identity, bool) {
	id, ok := ctx.Value(idKey{}).(identity)
	return id, ok
}

// Server is the MCP endpoint: auth + legacy discovery here, JSON-RPC in
// the SDK.
type Server struct {
	pool      *pgxpool.Pool
	adminPool *pgxpool.Pool // cross-tenant reads for admin tools (bypasses RLS)
	cache     *cache.Client
	auth      *authapi.Auth
	adminKey  string
	http      *mcpsrv.StreamableHTTPServer
}

func New(pool *pgxpool.Pool, cacheCli *cache.Client, auth *authapi.Auth, adminPool *pgxpool.Pool, adminKey string) *Server {
	s := &Server{pool: pool, cache: cacheCli, auth: auth, adminPool: adminPool, adminKey: adminKey}

	ms := mcpsrv.NewMCPServer("agents-proxy", "1.0.0", mcpsrv.WithToolFilter(filterByRole))

	// Business tools: the owner set is every business tool, each carrying
	// its REAL input schema now (the hand-rolled server advertised an
	// empty {} for all of them). The filter narrows per caller role.
	full := agentstools.NewToolSetFor(pool, "owner")
	for _, name := range full.Names() {
		t, ok := full.Get(name)
		if !ok {
			continue
		}
		schema := t.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		ms.AddTool(
			mcpsdk.NewToolWithRawSchema(t.Name, t.Description, schema),
			s.handleBusinessTool,
		)
	}
	// Admin tools, real schemas from admin.go.
	for _, d := range adminToolDefs() {
		raw, err := json.Marshal(d.InputSchema)
		if err != nil {
			raw = []byte(`{"type":"object"}`)
		}
		ms.AddTool(
			mcpsdk.NewToolWithRawSchema(d.Name, d.Description, raw),
			s.handleAdminTool,
		)
	}

	// Stateless: every POST stands alone, no session handshake required.
	// That matches the existing clients, which post bare JSON-RPC.
	s.http = mcpsrv.NewStreamableHTTPServer(ms, mcpsrv.WithStateLess(true))
	return s
}

// filterByRole is the access boundary: admin sees everything (it may
// call business tools with an explicit tenant), owner sees every
// business tool but never the admin ones, every other role sees exactly
// its allowlist. Unknown identity sees nothing. The SDK applies this to
// tools/list AND tools/call.
func filterByRole(ctx context.Context, tools []mcpsdk.Tool) []mcpsdk.Tool {
	id, ok := identityFromCtx(ctx)
	if !ok {
		return nil
	}
	if id.role == "admin" {
		return tools
	}
	// AllowedToolsFor("owner") is nil by convention (the full set, handled
	// by the caller) -- here that means every business tool.
	allowed := agentstools.AllowedToolsFor(id.role)
	out := make([]mcpsdk.Tool, 0, len(tools))
	for _, t := range tools {
		if strings.HasPrefix(t.Name, "admin_") {
			continue
		}
		if id.role == "owner" || allowed[t.Name] {
			out = append(out, t)
		}
	}
	return out
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		// Legacy discovery: tool names for the caller's role.
		tenantID, role, ok := s.resolve(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized: send Bearer ADMIN_API_KEY, X-Session-Token or X-Client-Token"})
			return
		}
		_ = tenantID
		writeJSON(w, http.StatusOK, map[string]any{"tools": s.discovery(role), "role": role})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	tenantID, role, ok := s.resolve(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	s.http.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), identity{tenantID: tenantID, role: role})))
}

// discovery lists {name, description} for a role, the same sets the
// filter exposes.
func (s *Server) discovery(role string) []map[string]any {
	out := []map[string]any{}
	if role == "admin" {
		for _, d := range adminToolDefs() {
			out = append(out, map[string]any{"name": d.Name, "description": d.Description})
		}
		return out
	}
	ts := agentstools.NewToolSetFor(s.pool, role)
	for _, name := range ts.Names() {
		t, ok := ts.Get(name)
		if !ok {
			continue
		}
		out = append(out, map[string]any{"name": t.Name, "description": t.Description})
	}
	return out
}

func (s *Server) handleBusinessTool(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	id, ok := identityFromCtx(ctx)
	if !ok {
		// Unreachable: ServeHTTP 401s before the SDK runs. Kept so a
		// missing identity fails closed, never open.
		return nil, errors.New("unauthorized")
	}
	tenantID := id.tenantID
	if id.role == "admin" {
		// Operator support: admin names the tenant per call.
		var a struct {
			TenantID string `json:"tenant_id"`
		}
		_ = json.Unmarshal(req.Params.RawArguments, &a)
		if a.TenantID == "" {
			return errorResult("error: admin calls need explicit tenant_id argument"), nil
		}
		tenantID = a.TenantID
	}
	ts := agentstools.NewToolSetFor(s.pool, mapRole(id.role))
	tool, ok := ts.Get(req.Params.Name)
	if !ok {
		return errorResult("error: unknown tool or not allowed for role " + id.role), nil
	}
	// Same RLS path as REST — tenant isolation inherited. RawArguments
	// may be nil when the caller sent none; tools treat that as
	// defaults, exactly like the old dispatch did.
	return textResult(tool.Run(ctx, tenantID, req.Params.RawArguments)), nil
}

func (s *Server) handleAdminTool(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	return textResult(s.runAdminTool(ctx, req.Params.Name, req.Params.RawArguments)), nil
}

func textResult(text string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{
			mcpsdk.TextContent{Type: "text", Text: text},
		},
	}
}

func errorResult(text string) *mcpsdk.CallToolResult {
	r := textResult(text)
	r.IsError = true
	return r
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

// toolDef is one admin tool description (admin.go owns the list).
type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
