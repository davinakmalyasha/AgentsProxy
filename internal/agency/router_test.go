package agency

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// The agency plane must be pure chi: building the router must not panic
// (chi panics on conflicting wildcard patterns at registration), and the
// key mounts from every domain must exist.
func TestRouter_ChiRoutesRegistered(t *testing.T) {
	r := NewRouter(Deps{})
	var paths []string
	if err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		paths = append(paths, method+" "+route)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	for _, want := range []string{
		"GET /packages",
		"GET /packages/{slug}",
		"GET /projects/{slug}",
		"GET /articles",
		"GET /promos/active",
		"GET /settings",
		"POST /leads",
		"POST /orders",
		"GET /orders/track/{orderId}",
		"POST /auth/login",
		"GET /auth/me",
		"POST /auth/change-password",
		"GET /users",
		"GET /orders/{id}",
		"PATCH /orders/{segment}/{key}",
		"DELETE /orders/milestones/{id}",
		"GET /hosting",
		"GET /hosting/stats",
		"POST /packages",
		"GET /dashboard",
		"GET /audit",
		"POST /webhooks",
		"GET /client/orders",
		"POST /client/requests",
		"PATCH /client/admin/requests/{id}",
		"GET /agent/leads",
		"POST /agent/whatsapp/send",
		"POST /upload",
		"GET /whatsapp/status",
		"POST /ai/generate-copy",
	} {
		found := false
		for _, p := range paths {
			if p == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing agency route %q", want)
		}
	}
}

// Guard-first behavior must hold without a database: malformed/unauthenticated
// requests fail in middleware or validation, before any store call.
func TestRouter_GuardsBeforeStores(t *testing.T) {
	r := NewRouter(Deps{})
	serve := func(method, target string, body string, headers map[string]string) int {
		var reader *strings.Reader
		if body == "" {
			reader = strings.NewReader("")
		} else {
			reader = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, target, reader)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := serve("POST", "/leads", "", nil); code != http.StatusBadRequest {
		t.Errorf("POST /leads empty body = %d, want 400 (no DB hit)", code)
	}
	if code := serve("GET", "/auth/me", "", nil); code != http.StatusUnauthorized {
		t.Errorf("GET /auth/me no cookie = %d, want 401", code)
	}
	if code := serve("GET", "/dashboard", "", nil); code != http.StatusUnauthorized {
		t.Errorf("GET /dashboard no cookie = %d, want 401", code)
	}
	if code := serve("GET", "/users", "", nil); code != http.StatusUnauthorized {
		t.Errorf("GET /users no cookie = %d, want 401", code)
	}
	if code := serve("GET", "/agent/leads", "", nil); code != http.StatusUnauthorized {
		t.Errorf("GET /agent/leads no key = %d, want 401", code)
	}
	if code := serve("GET", "/client/orders", "", nil); code != http.StatusUnauthorized {
		t.Errorf("GET /client/orders no cookie = %d, want 401", code)
	}
}
