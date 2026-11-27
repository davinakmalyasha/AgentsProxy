// Package mcpclient connects a project's agent to external MCP servers
// declared in projects/<slug>/agent/mcp.json.
//
// The project config is build-time data (embedded and validated at boot), so
// the MCP endpoint is trusted in the same way a Go handler is. The runtime
// still treats every remote response as untrusted text: results are capped,
// calls are bounded, and no header value is ever logged.
package mcpclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
	"github.com/davinakmalyasha/agents-proxy/internal/httpx"
)

const (
	connectTimeout = 10 * time.Second
	callTimeout    = 15 * time.Second
	maxResultRunes = 16_000
	maxTools       = 50
	retryAfter     = 30 * time.Second
)

// ServerAllowsRole reports whether a tier may use this server at all. An
// empty role list means staff only, never the public widget. Filtering before
// connecting matters: an anon turn must not open sessions for staff-only
// servers just to discard their tools.
func ServerAllowsRole(srv agentconfig.MCPServer, role string) bool {
	if len(srv.Roles) == 0 {
		return role == "owner" || role == "pegawai"
	}
	for _, r := range srv.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// Tool is one remote tool plus the tiers allowed to see it. Roles is empty
// for the safe default: staff only.
type Tool struct {
	agentstools.Tool
	Roles map[string]bool
}

// Allowed reports whether a tools-role may see this tool. External tools are
// an action surface, so an empty Roles list means owner + pegawai, never the
// public landing widget.
func (t Tool) Allowed(role string) bool {
	if len(t.Roles) == 0 {
		return role == "owner" || role == "pegawai"
	}
	return t.Roles[role]
}

// Manager owns one connection set per project and closes them on config
// reload or shutdown. A nil Manager is safe: no servers, no tools.
type Manager struct {
	mu       sync.Mutex
	projects map[string]*projectEntry
	lookup   func(string) (string, bool)
	now      func() time.Time
}

type projectEntry struct {
	mu          sync.Mutex
	fingerprint string
	tools       []Tool
	sessions    []*mcp.ClientSession
	failed      bool
	retryAt     time.Time
}

// NewManager builds a manager that resolves ${ENV} references with
// os.LookupEnv.
func NewManager() *Manager {
	return NewManagerWithLookup(os.LookupEnv)
}

// NewManagerWithLookup is the testable constructor. lookup may be nil.
func NewManagerWithLookup(lookup func(string) (string, bool)) *Manager {
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	return &Manager{
		projects: map[string]*projectEntry{},
		lookup:   lookup,
		now:      time.Now,
	}
}

// Tools returns the project's current MCP tools. A server that is down is
// logged and skipped; native tools still work. Failures are retried after a
// short backoff instead of adding a connection timeout to every turn.
func (m *Manager) Tools(ctx context.Context, project, role string, servers []agentconfig.MCPServer) []Tool {
	if m == nil || len(servers) == 0 {
		return nil
	}
	fp := fingerprint(servers)
	e := m.entry(project, role)
	e.mu.Lock()
	defer e.mu.Unlock()

	now := m.now()
	if e.fingerprint == fp && (!e.failed || now.Before(e.retryAt)) {
		return e.tools
	}

	e.closeLocked()
	e.fingerprint = fp
	e.tools = nil
	e.failed = false

	used := map[string]bool{}
	for _, srv := range servers {
		tools, session, err := connect(ctx, project, srv, m.lookup, used)
		if err != nil {
			e.failed = true
			slog.Warn("mcp: server unavailable",
				"project", project, "server", srv.Name, "err", redact(err.Error()))
			continue
		}
		e.sessions = append(e.sessions, session)
		e.tools = append(e.tools, tools...)
		if len(e.tools) >= maxTools {
			e.tools = e.tools[:maxTools]
			break
		}
	}
	if e.failed {
		e.retryAt = now.Add(retryAfter)
	}
	return e.tools
}

// RefreshDue reports whether the cached tool set is stale: config changed,
// or a failed server is past its retry backoff. The runner uses it to evict
// its own per-(tenant, role) cache.
func (m *Manager) RefreshDue(project, role string, servers []agentconfig.MCPServer) bool {
	if m == nil || len(servers) == 0 {
		return false
	}
	e := m.entry(project, role)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fingerprint != fingerprint(servers) {
		return true
	}
	return e.failed && !m.now().Before(e.retryAt)
}

// CloseProject drops one project's sessions; the next Tools call reconnects.
// Called when an admin reload replaces the project's config.
func (m *Manager) CloseProject(project string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	var entries []*projectEntry
	prefix := project + "\x00"
	for key, e := range m.projects {
		if strings.HasPrefix(key, prefix) {
			entries = append(entries, e)
			delete(m.projects, key)
		}
	}
	m.mu.Unlock()
	for _, e := range entries {
		e.mu.Lock()
		e.closeLocked()
		e.mu.Unlock()
	}
}

// Close drops every session. Safe to call more than once.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	entries := make([]*projectEntry, 0, len(m.projects))
	for _, e := range m.projects {
		entries = append(entries, e)
	}
	m.mu.Unlock()
	for _, e := range entries {
		e.mu.Lock()
		e.closeLocked()
		e.mu.Unlock()
	}
}

func (m *Manager) entry(project, role string) *projectEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.projects == nil {
		m.projects = map[string]*projectEntry{}
	}
	key := project + "\x00" + role
	e := m.projects[key]
	if e == nil {
		e = &projectEntry{}
		m.projects[key] = e
	}
	return e
}

func (e *projectEntry) closeLocked() {
	for _, s := range e.sessions {
		_ = s.Close()
	}
	e.sessions = nil
}

func fingerprint(servers []agentconfig.MCPServer) string {
	raw, err := json.Marshal(servers)
	if err != nil {
		return fmt.Sprintf("%#v", servers)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func connect(ctx context.Context, project string, srv agentconfig.MCPServer, lookup func(string) (string, bool), used map[string]bool) ([]Tool, *mcp.ClientSession, error) {
	endpoint, err := expandEnv(srv.URL, lookup)
	if err != nil {
		return nil, nil, fmt.Errorf("url: %w", err)
	}
	headers := make(map[string]string, len(srv.Headers))
	for name, value := range srv.Headers {
		v, err := expandEnv(value, lookup)
		if err != nil {
			return nil, nil, fmt.Errorf("header %s: %w", name, err)
		}
		headers[name] = v
	}

	// The per-request contexts below carry the real 10s/15s bounds. A
	// shorter client-wide timeout would cut a tool call before its own
	// deadline; no timeout lets the context do its job.
	client := &http.Client{
		Transport: &headerTransport{base: httpx.SharedTransport(), headers: headers},
	}
	mcpClient := mcp.NewClient(&mcp.Implementation{
		Name:    "agents-proxy",
		Version: "1.0",
	}, nil)
	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	session, err := mcpClient.Connect(connectCtx, &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           client,
		DisableStandaloneSSE: true, // request/response only; no long-lived connection
		MaxRetries:           1,
	}, nil)
	if err != nil {
		return nil, nil, err
	}

	listCtx, listCancel := context.WithTimeout(ctx, connectTimeout)
	defer listCancel()
	res, err := session.ListTools(listCtx, nil)
	if err != nil {
		_ = session.Close()
		return nil, nil, err
	}

	allow := map[string]bool{}
	for _, name := range srv.Tools {
		allow[name] = true
	}
	roles := map[string]bool{}
	for _, role := range srv.Roles {
		roles[role] = true
	}
	if len(roles) == 0 {
		roles = nil
	}

	out := make([]Tool, 0, len(res.Tools))
	for _, remote := range res.Tools {
		if len(allow) > 0 && !allow[remote.Name] {
			continue
		}
		name := namespacedName(srv.Name, remote.Name, used)
		schema := json.RawMessage(`{"type":"object"}`)
		if remote.InputSchema != nil {
			if raw, err := json.Marshal(remote.InputSchema); err == nil && len(raw) > 0 {
				schema = raw
			}
		}
		desc := strings.TrimSpace(remote.Description)
		if desc == "" {
			desc = "External tool " + remote.Name + " on " + srv.Name
		}
		mutating := true
		if remote.Annotations != nil && remote.Annotations.ReadOnlyHint {
			mutating = false
		}
		out = append(out, Tool{
			Tool: agentstools.Tool{
				Name:        name,
				Description: desc,
				Parameters:  schema,
				Tenant:      project,
				Mutating:    mutating,
				Run:         callFunc(session, remote.Name),
			},
			Roles: roles,
		})
	}
	return out, session, nil
}

func callFunc(session *mcp.ClientSession, remoteName string) func(context.Context, string, json.RawMessage) string {
	return func(ctx context.Context, _ string, args json.RawMessage) string {
		var in map[string]any
		if len(args) > 0 {
			if err := json.Unmarshal(args, &in); err != nil {
				return "Error: invalid arguments: " + err.Error()
			}
		}
		if in == nil {
			in = map[string]any{}
		}
		callCtx, cancel := context.WithTimeout(ctx, callTimeout)
		defer cancel()
		res, err := session.CallTool(callCtx, &mcp.CallToolParams{Name: remoteName, Arguments: in})
		if err != nil {
			return "Error: MCP call failed: " + redact(err.Error())
		}
		return flatten(res)
	}
}

func flatten(res *mcp.CallToolResult) string {
	if res == nil {
		return "Error: empty MCP response"
	}
	var b strings.Builder
	for _, content := range res.Content {
		switch v := content.(type) {
		case *mcp.TextContent:
			b.WriteString(v.Text)
		default:
			raw, err := json.Marshal(content)
			if err == nil {
				b.Write(raw)
			}
		}
	}
	if b.Len() == 0 && res.StructuredContent != nil {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			b.Write(raw)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		out = "(empty result)"
	}
	if res.IsError {
		out = "Error: " + out
	}
	return truncate(out, maxResultRunes)
}

func namespacedName(server, tool string, used map[string]bool) string {
	base := "mcp_" + sanitizeName(server) + "_" + sanitizeName(tool)
	if len(base) > 64 {
		sum := sha256.Sum256([]byte(server + "\x00" + tool))
		base = base[:55] + "_" + hex.EncodeToString(sum[:4])
	}
	name := base
	for i := 2; used[name]; i++ {
		suffix := fmt.Sprintf("_%d", i)
		trim := 64 - len(suffix)
		if trim < len(base) {
			name = base[:trim] + suffix
		} else {
			name = base + suffix
		}
	}
	used[name] = true
	return name
}

func sanitizeName(v string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(v) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_-")
	if out == "" {
		return "x"
	}
	return out
}

func truncate(v string, max int) string {
	if utf8.RuneCountInString(v) <= max {
		return v
	}
	runes := []rune(v)
	return string(runes[:max]) + "…"
}

var (
	envRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	urlRe    = regexp.MustCompile(`https?://[^\s"']+`)
)

func expandEnv(v string, lookup func(string) (string, bool)) (string, error) {
	var missing string
	out := envRefRe.ReplaceAllStringFunc(v, func(ref string) string {
		name := envRefRe.FindStringSubmatch(ref)[1]
		value, ok := lookup(name)
		if !ok || value == "" {
			missing = name
			return ref
		}
		return value
	})
	if missing != "" {
		return "", fmt.Errorf("environment variable %s is not set", missing)
	}
	return out, nil
}

func redact(v string) string {
	return urlRe.ReplaceAllString(v, "[url]")
}

type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for name, value := range t.headers {
		clone.Header.Set(name, value)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}
