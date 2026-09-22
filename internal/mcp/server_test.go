package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPBearerAdmin(t *testing.T) {
	// No DB pools: only auth + initialize/tools/list paths run.
	srv := New(nil, nil, nil, nil, "admin-secret")
	r := httptest.NewRequest("POST", "/v1/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	r.Header.Set("Authorization", "Bearer admin-secret")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("initialize: got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"2025-06-18"`) {
		t.Fatalf("missing protocol version: %s", w.Body.String())
	}
	// tools/list as admin must expose admin_* tools.
	r2 := httptest.NewRequest("POST", "/v1/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`))
	r2.Header.Set("Authorization", "Bearer admin-secret")
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, r2)
	if !strings.Contains(w2.Body.String(), "admin_proxy_health") {
		t.Fatalf("admin tools missing: %s", w2.Body.String())
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

func TestSplitDef(t *testing.T) {
	name, desc := splitDef("- book_slot: Book a facility slot.")
	if name != "book_slot" || desc != "Book a facility slot." {
		t.Fatalf("got %q / %q", name, desc)
	}
}

func TestSchemaHelpers(t *testing.T) {
	p := schemaProps(map[string]any{"a": strProp("x")}, "a")
	b, _ := json.Marshal(p)
	s := string(b)
	if !strings.Contains(s, `"required":["a"]`) || !strings.Contains(s, `"type":"object"`) {
		t.Fatalf("schema: %s", s)
	}
}
