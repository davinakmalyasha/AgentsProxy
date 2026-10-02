package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"

	_ "github.com/davinakmalyasha/agents-proxy/AgentsTools/CentrumBadminton"
)

func TestMCPBearerAdmin(t *testing.T) {
	// No DB pools: only auth + initialize/tools/list paths run.
	srv := New(nil, nil, nil, nil, "admin-secret")
	// A real client sends its version; the SDK must echo a version it
	// supports back (negotiation, not a pinned string).
	r := httptest.NewRequest("POST", "/v1/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`))
	r.Header.Set("Authorization", "Bearer admin-secret")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("initialize: got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"2025-06-18"`) {
		t.Fatalf("version not negotiated: %s", w.Body.String())
	}
	// tools/list as admin must expose admin_* tools.
	r2 := httptest.NewRequest("POST", "/v1/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`))
	r2.Header.Set("Authorization", "Bearer admin-secret")
	r2.Header.Set("Content-Type", "application/json")
	r2.Header.Set("Accept", "application/json, text/event-stream")
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, r2)
	if !strings.Contains(w2.Body.String(), "admin_proxy_health") {
		t.Fatalf("admin tools missing: %s", w2.Body.String())
	}
}

// TestMCPBusinessSchemasAreReal pins the reason for the SDK swap: the
// hand-rolled server advertised an empty {} schema for every business
// tool. Now query_income carries its from/to contract.
func TestMCPBusinessSchemasAreReal(t *testing.T) {
	srv := New(nil, nil, nil, nil, "admin-secret")
	r := httptest.NewRequest("POST", "/v1/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":6,"method":"tools/list","params":{}}`))
	r.Header.Set("Authorization", "Bearer admin-secret")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("tools/list: got %d", w.Code)
	}
	var resp struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, tool := range resp.Result.Tools {
		if tool.Name != "query_income" {
			continue
		}
		props, ok := tool.InputSchema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("query_income has no properties: %v", tool.InputSchema)
		}
		if _, ok := props["from"]; !ok {
			t.Errorf("query_income schema missing from: %v", props)
		}
		if _, ok := props["to"]; !ok {
			t.Errorf("query_income schema missing to: %v", props)
		}
		return
	}
	t.Error("query_income not in admin tool list")
}

// database: the admin tool runs and reports the missing store, which
// means auth → filter → handler all fired inside the SDK.
func TestMCPAdminCallThroughSDK(t *testing.T) {
	srv := New(nil, nil, nil, nil, "admin-secret")
	r := httptest.NewRequest("POST", "/v1/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"admin_proxy_health","arguments":{}}}`))
	r.Header.Set("Authorization", "Bearer admin-secret")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("tools/call: got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "admin store unavailable") {
		t.Fatalf("admin tool did not run through the SDK: %s", w.Body.String())
	}
}

// TestToolFilter pins the access boundary directly: admin sees admin
// tools, client sees only its allowlist (real schemas now, not {}),
// unknown identity sees nothing.
func TestToolFilter(t *testing.T) {
	mkTools := []mcpsdk.Tool{
		{Name: "admin_proxy_health"},
		{Name: "query_income"},
		{Name: "get_pricing"},
	}
	withRole := func(role string) context.Context {
		if role == "" {
			return context.Background()
		}
		return withIdentity(context.Background(), identity{tenantID: "t", role: role})
	}
	if got := filterByRole(withRole("admin"), mkTools); len(got) != 3 {
		t.Errorf("admin sees %d tools, want all 3", len(got))
	}
	got := filterByRole(withRole("client"), mkTools)
	if len(got) != 1 || got[0].Name != "get_pricing" {
		names := []string{}
		for _, t2 := range got {
			names = append(names, t2.Name)
		}
		t.Errorf("client sees %v, want only get_pricing", names)
	}
	if got := filterByRole(withRole("owner"), mkTools); len(got) != 2 {
		t.Errorf("owner sees %d tools, want the 2 business ones", len(got))
	}
	if got := filterByRole(withRole(""), mkTools); len(got) != 0 {
		t.Errorf("unknown identity sees %d tools, want none", len(got))
	}
}

func TestMCPUnauthorized(t *testing.T) {
	srv := New(nil, nil, nil, nil, "admin-secret")
	r := httptest.NewRequest("POST", "/v1/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"admin_proxy_health"}}`))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauth tools/call: got %d", w.Code)
	}
	// Wrong key.
	r2 := httptest.NewRequest("POST", "/v1/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{}}`))
	r2.Header.Set("Authorization", "Bearer wrong")
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, r2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key tools/list: got %d", w2.Code)
	}
}

// TestSchemaHelpers pins the admin schema builder both servers share.

func TestSchemaHelpers(t *testing.T) {
	p := schemaProps(map[string]any{"a": strProp("x")}, "a")
	b, _ := json.Marshal(p)
	s := string(b)
	if !strings.Contains(s, `"required":["a"]`) || !strings.Contains(s, `"type":"object"`) {
		t.Fatalf("schema: %s", s)
	}
}
