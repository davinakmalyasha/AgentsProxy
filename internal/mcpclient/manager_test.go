package mcpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
)

func testMCPServer(t *testing.T, hits *atomic.Int32, wantAuth string) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "echo",
		Description: "echo text",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if hits != nil {
			hits.Add(1)
		}
		var in struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "echo:" + in.Text}},
		}, nil
	})
	server.AddTool(&mcp.Tool{
		Name:        "secret",
		Description: "must be filtered by the allow-list",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "secret"}}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantAuth != "" && r.Header.Get("Authorization") != wantAuth {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
}

func TestToolsListsCallsAndFiltersByAllowList(t *testing.T) {
	srv := testMCPServer(t, nil, "")
	defer srv.Close()

	m := NewManagerWithLookup(nil)
	defer m.Close()
	servers := []agentconfig.MCPServer{{Name: "echo", URL: srv.URL, Tools: []string{"echo"}}}
	tools := m.Tools(context.Background(), "acme", servers)
	if len(tools) != 1 {
		t.Fatalf("tools=%d, want 1", len(tools))
	}
	tool := tools[0]
	if tool.Name != "mcp_echo_echo" {
		t.Errorf("name=%q", tool.Name)
	}
	if !tool.Allowed("owner") || !tool.Allowed("pegawai") {
		t.Error("staff should see MCP tools")
	}
	if tool.Allowed("anon") || tool.Allowed("client") {
		t.Error("public/client tiers must not see MCP tools by default")
	}
	if !tool.Mutating {
		t.Error("unknown remote tools default to mutating")
	}
	out := tool.Run(context.Background(), "acme", json.RawMessage(`{"text":"hi"}`))
	if out != "echo:hi" {
		t.Errorf("result=%q", out)
	}
}

func TestToolsResolveHeaderEnvironmentAndSkipWhenMissing(t *testing.T) {
	srv := testMCPServer(t, nil, "Bearer token-123")
	defer srv.Close()

	m := NewManagerWithLookup(func(name string) (string, bool) {
		if name == "MCP_TOKEN" {
			return "token-123", true
		}
		return "", false
	})
	defer m.Close()
	servers := []agentconfig.MCPServer{{
		Name:    "echo",
		URL:     srv.URL,
		Tools:   []string{"echo"},
		Headers: map[string]string{"Authorization": "Bearer ${MCP_TOKEN}"},
	}}
	if tools := m.Tools(context.Background(), "acme", servers); len(tools) != 1 {
		t.Fatalf("tools=%d, want 1", len(tools))
	}

	var hits atomic.Int32
	srv2 := testMCPServer(t, &hits, "Bearer token-123")
	defer srv2.Close()
	m2 := NewManagerWithLookup(func(string) (string, bool) { return "", false })
	defer m2.Close()
	servers2 := []agentconfig.MCPServer{{
		Name:    "echo",
		URL:     srv2.URL,
		Tools:   []string{"echo"},
		Headers: map[string]string{"Authorization": "Bearer ${MCP_TOKEN}"},
	}}
	if tools := m2.Tools(context.Background(), "acme", servers2); len(tools) != 0 {
		t.Fatalf("tools=%d, want 0 when the token is absent", len(tools))
	}
	if hits.Load() != 0 {
		t.Fatal("server was called without a token")
	}
}

func TestRefreshDueOnConfigChange(t *testing.T) {
	srv := testMCPServer(t, nil, "")
	defer srv.Close()
	m := NewManagerWithLookup(nil)
	defer m.Close()
	servers := []agentconfig.MCPServer{{Name: "echo", URL: srv.URL}}
	_ = m.Tools(context.Background(), "acme", servers)
	if m.RefreshDue("acme", servers) {
		t.Fatal("unchanged config should not be due")
	}
	servers[0].Tools = []string{"echo"}
	if !m.RefreshDue("acme", servers) {
		t.Fatal("changed config should be due")
	}
}

func TestExpandEnvRejectsUnsetReference(t *testing.T) {
	if _, err := expandEnv("Bearer ${NOPE}", func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("unset reference was accepted")
	}
	v, err := expandEnv("Bearer ${TOKEN}", func(string) (string, bool) { return "abc", true })
	if err != nil || v != "Bearer abc" {
		t.Fatalf("v=%q err=%v", v, err)
	}
}
