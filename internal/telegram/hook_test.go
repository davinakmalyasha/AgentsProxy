package telegram

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Hook auth without any agent/DB: unknown bot → 404, wrong secret → 403.
func TestServeHookAuth(t *testing.T) {
	m := NewManager(nil, nil, nil, "", false, 0)
	r := chi.NewRouter()
	r.Post("/telegram/hook/{id}", m.ServeHook)

	// Unknown bot.
	req := httptest.NewRequest("POST", "/telegram/hook/00000000-0000-0000-0000-000000000000",
		strings.NewReader(`{"update_id":1}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown bot: got %d want 404", rec.Code)
	}

	// Known bot, wrong secret (inject directly, no DB).
	m.bots["11111111-1111-1111-1111-111111111111"] = &ManagedBot{
		ID: "11111111-1111-1111-1111-111111111111", WebhookSecret: "s3cret",
	}
	req2 := httptest.NewRequest("POST", "/telegram/hook/11111111-1111-1111-1111-111111111111",
		strings.NewReader(`{"update_id":2}`))
	req2.Header.Set("X-Telegram-Bot-Api-Secret-Token", "wrong")
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("bad secret: got %d want 403", rec2.Code)
	}
}
