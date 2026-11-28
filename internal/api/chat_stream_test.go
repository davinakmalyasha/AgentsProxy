package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/llm"
)

// sseChatMock serves one canned OpenAI-compatible stream: two content
// deltas (split mid-word), a usage chunk, then [DONE]. status != 200
// serves a plain error body instead.
func sseChatMock(t *testing.T, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 200 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("provider blew up"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"stream\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ed reply\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":18}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}

func streamChatHandler(t *testing.T, h *harness, llmURL string) http.HandlerFunc {
	t.Helper()
	lc := llm.New(llmURL, "sk-test", "mock-model", 1024)
	cli := &cache.Client{DB: h.pool, RDB: h.redis}
	chatH := NewChat(h.pool, cli, lc, h.redis, 100000)
	return chatH.ServeStream
}

// readSSE splits a served event stream into its data payloads.
func readSSE(t *testing.T, resp *http.Response) []string {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, block := range strings.Split(string(raw), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		if !strings.HasPrefix(block, "data:") {
			t.Fatalf("non-data block: %q", block)
		}
		out = append(out, strings.TrimSpace(strings.TrimPrefix(block, "data:")))
	}
	return out
}

func TestChatStream_Happy(t *testing.T) {
	h := setup(t)
	srv := sseChatMock(t, 200)
	defer srv.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/stream", streamChatHandler(t, h, srv.URL))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := `{"client_token":"tok-alpha","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", ts.URL+"/v1/chat/stream", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	resp := mustReq(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(raw))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type=%q want text/event-stream", ct)
	}
	events := readSSE(t, resp)
	if len(events) != 3 {
		t.Fatalf("events=%d (%v) want 3 (2 deltas + done)", len(events), events)
	}
	var d0, d1 map[string]string
	if err := json.Unmarshal([]byte(events[0]), &d0); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(events[1]), &d1); err != nil {
		t.Fatal(err)
	}
	if d0["delta"]+d1["delta"] != "streamed reply" {
		t.Errorf("deltas=%q+%q", d0["delta"], d1["delta"])
	}
	var done map[string]any
	if err := json.Unmarshal([]byte(events[2]), &done); err != nil {
		t.Fatal(err)
	}
	if done["done"] != true {
		t.Errorf("done=%v", done)
	}
	if done["reply"] != "streamed reply" {
		t.Errorf("reply=%v", done["reply"])
	}
	usage, _ := done["usage"].(map[string]any)
	if usage["prompt_tokens"] != 12.0 || usage["completion_tokens"] != 18.0 {
		t.Errorf("usage=%v", usage)
	}
	// Metered like the non-streaming path.
	var n int
	h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM usage u JOIN clients c ON c.id = u.client_id WHERE c.tenant_id='alpha' AND u.kind='chat'`,
	).Scan(&n)
	if n < 1 {
		t.Errorf("usage rows=%d want >= 1", n)
	}
}

func TestChatStream_InvalidToken(t *testing.T) {
	h := setup(t)
	srv := sseChatMock(t, 200)
	defer srv.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/stream", streamChatHandler(t, h, srv.URL))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Pre-stream failures keep the JSON contract + status of /v1/chat.
	body := `{"client_token":"not-a-real-token","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", ts.URL+"/v1/chat/stream", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	resp := mustReq(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status=%d want 401", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type=%q want application/json", ct)
	}
}

func TestChatStream_ProviderError(t *testing.T) {
	h := setup(t)
	srv := sseChatMock(t, 500)
	defer srv.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/stream", streamChatHandler(t, h, srv.URL))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Headers are already sent (200) when the provider fails, so the
	// failure arrives as an event, not a status.
	body := `{"client_token":"tok-alpha","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", ts.URL+"/v1/chat/stream", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	resp := mustReq(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status=%d want 200 (error arrives as event)", resp.StatusCode)
	}
	events := readSSE(t, resp)
	if len(events) != 1 {
		t.Fatalf("events=%v want single error event", events)
	}
	var e map[string]string
	if err := json.Unmarshal([]byte(events[0]), &e); err != nil {
		t.Fatal(err)
	}
	if e["error"] == "" {
		t.Errorf("event=%v want error", e)
	}
}
