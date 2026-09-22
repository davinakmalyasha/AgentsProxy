// Tests for the Telegram channel. Uses a fake Bot API server so no
// real Telegram token or network is needed.
//
// DONE: Phase 5 (telegram agentic) / STANDARDS A
package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeAPIServer serves the minimal Bot API surface the Bot client
// uses: /sendMessage and /getUpdates.
func fakeAPIServer(t *testing.T, sent *[]map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		switch r.URL.Path {
		case "/botTEST/sendMessage":
			*sent = append(*sent, body)
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
		case "/botTEST/getUpdates":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
}

func TestSend(t *testing.T) {
	var sent []map[string]any
	srv := fakeAPIServer(t, &sent)
	defer srv.Close()

	b := New("TEST")
	b.HTTP = srv.Client()
	// Point the client at the fake server.
	oldBase := apiBase
	apiBase = srv.URL + "/bot%s"
	defer func() { apiBase = oldBase }()

	if err := b.Send(context.Background(), 12345, "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(sent) != 1 {
		t.Fatalf("sent=%d want 1", len(sent))
	}
	if sent[0]["chat_id"] != float64(12345) || sent[0]["text"] != "hello" {
		t.Errorf("payload=%v", sent[0])
	}
}

func TestGetUpdates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true,"result":[
			{"update_id":1,"message":{"message_id":10,"chat":{"id":111},"text":"book court 3"}},
			{"update_id":2,"message":{"message_id":11,"chat":{"id":222},"text":"income?"}}
		]}`))
	}))
	defer srv.Close()

	b := New("TEST")
	b.HTTP = srv.Client()
	oldBase := apiBase
	apiBase = srv.URL + "/bot%s"
	defer func() { apiBase = oldBase }()

	ups, err := b.GetUpdates(context.Background(), 0)
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	if len(ups) != 2 {
		t.Fatalf("updates=%d want 2", len(ups))
	}
	if ups[0].Message.Chat.ID != 111 || ups[0].Message.Text != "book court 3" {
		t.Errorf("update[0]=%+v", ups[0])
	}
	if b.Offset != 3 {
		t.Errorf("offset=%d want 3", b.Offset)
	}
}
