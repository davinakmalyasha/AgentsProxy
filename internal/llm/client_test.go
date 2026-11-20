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
