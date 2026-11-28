package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestChat_Happy(t *testing.T) {
	// Mock DeepSeek-style response.
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != chatCompletionsPath {
			t.Errorf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("auth=%q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("x-opencode-session") == "" {
			t.Error("missing x-opencode-session header")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "test",
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "hi back"}}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 20},
		})
	}))
	defer mock.Close()

	c := New(mock.URL, "test-key", "mock-model", 2048)
	reply, inTok, outTok, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "hi back" {
		t.Errorf("reply=%q", reply)
	}
	if inTok != 10 || outTok != 20 {
		t.Errorf("tokens=%d/%d", inTok, outTok)
	}
}

func TestChat_OverrideModel(t *testing.T) {
	var gotModel string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body chatRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "ok"}}},
			"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1},
		})
	}))
	defer mock.Close()

	c := New(mock.URL, "k", "default", 1024)
	_, _, _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "x"}}, "override-model")
	if err != nil {
		t.Fatal(err)
	}
	if gotModel != "override-model" {
		t.Errorf("model=%q", gotModel)
	}
}

func TestChat_ProviderError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("internal error from provider"))
	}))
	defer mock.Close()

	c := New(mock.URL, "k", "m", 100)
	_, _, _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "x"}}, "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errorsIs(err, ErrProvider) {
		t.Errorf("err=%v want ErrProvider", err)
	}
}

func TestChat_Timeout_NoLeak(t *testing.T) {
	// Unreachable IP — should error within a few seconds, not hang.
	c := New("http://127.0.0.1:1", "k", "m", 100)
	c.HTTP.Timeout = 2_000_000_000 // 2s in nanos
	_, _, _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "x"}}, "")
	if err == nil {
		t.Fatal("expected error")
	}
}

// sseMock serves one canned OpenAI-compatible SSE stream: two content
// deltas (split mid-word), a usage chunk, then [DONE].
func sseMock(t *testing.T, chunks []string, usage string, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 200 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		var payload chatRequest
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if !payload.Stream {
			t.Error("expected stream:true")
		}
		if payload.StreamOptions == nil || !payload.StreamOptions.IncludeUsage {
			t.Error("expected stream_options.include_usage")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":" + quoteJSON(t, c) + "}}]}\n\n"))
		}
		if usage != "" {
			_, _ = w.Write([]byte("data: " + usage + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}

func quoteJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestStream_Happy(t *testing.T) {
	srv := sseMock(t,
		[]string{"hel", "lo back"},
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20}}`,
		200, "")
	defer srv.Close()

	c := New(srv.URL, "k", "mock-model", 2048)
	var deltas []string
	reply, inTok, outTok, err := c.Stream(context.Background(),
		[]Message{{Role: "user", Content: "hi"}},
		"",
		func(d string) error { deltas = append(deltas, d); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if reply != "hello back" {
		t.Errorf("reply=%q", reply)
	}
	if len(deltas) != 2 || deltas[0]+deltas[1] != "hello back" {
		t.Errorf("deltas=%q", deltas)
	}
	if inTok != 10 || outTok != 20 {
		t.Errorf("tokens=%d/%d", inTok, outTok)
	}
}

func TestStream_NoUsageChunk_FallsBack(t *testing.T) {
	srv := sseMock(t, []string{"abcd"}, "", 200, "")
	defer srv.Close()

	c := New(srv.URL, "k", "mock-model", 2048)
	reply, _, outTok, err := c.Stream(context.Background(),
		[]Message{{Role: "user", Content: "hi"}}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply != "abcd" {
		t.Errorf("reply=%q", reply)
	}
	if outTok != 1 { // len("abcd")/4
		t.Errorf("outTok=%d want 1", outTok)
	}
}

func TestStream_ProviderError(t *testing.T) {
	srv := sseMock(t, nil, "", 500, "internal error from provider")
	defer srv.Close()

	c := New(srv.URL, "k", "m", 100)
	_, _, _, err := c.Stream(context.Background(), []Message{{Role: "user", Content: "x"}}, "", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errorsIs(err, ErrProvider) {
		t.Errorf("err=%v want ErrProvider", err)
	}
}

func TestStream_InvalidOption_Retries(t *testing.T) {
	var models, reasons []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload chatRequest
		_ = json.NewDecoder(r.Body).Decode(&payload)
		models = append(models, payload.Model)
		if payload.ReasoningEffort != nil {
			reasons = append(reasons, *payload.ReasoningEffort)
		}
		if payload.ReasoningEffort != nil && *payload.ReasoningEffort == "low" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte("Invalid option"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := New(srv.URL, "k", "mock-model", 100)
	reply, _, _, err := c.Stream(context.Background(), []Message{{Role: "user", Content: "x"}}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply != "ok" {
		t.Errorf("reply=%q", reply)
	}
	if len(reasons) != 2 || reasons[0] != "low" || reasons[1] != "none" {
		t.Errorf("reasons=%v want [low none]", reasons)
	}
}

func TestStream_OnDeltaAbort_KeepsAccumulated(t *testing.T) {
	srv := sseMock(t, []string{"ab", "cd"}, "", 200, "")
	defer srv.Close()

	c := New(srv.URL, "k", "mock-model", 100)
	calls := 0
	reply, _, outTok, err := c.Stream(context.Background(),
		[]Message{{Role: "user", Content: "x"}}, "",
		func(d string) error {
			calls++
			return context.Canceled // simulate dead client on first delta
		})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("calls=%d want 1", calls)
	}
	if reply != "ab" {
		t.Errorf("reply=%q want accumulated prefix", reply)
	}
	if outTok != 0 { // len("ab")/4 == 0; handler skips metering on zeros
		t.Errorf("outTok=%d want 0", outTok)
	}
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrap interface{ Unwrap() error }
		if u, ok := err.(unwrap); ok {
			err = u.Unwrap()
			continue
		}
		break
	}
	return false
}
