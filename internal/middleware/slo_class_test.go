package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSLOClass(t *testing.T) {
	cases := []struct {
		method, path, want string
	}{
		{http.MethodGet, "/healthz", ""},
		{http.MethodGet, "/v1/admin/clients", "admin"},
		{http.MethodGet, "/v1/admin/slo", "admin"},
		{http.MethodPost, "/v1/admin/clients", "admin"},
		{http.MethodPost, "/v1/agent/chat", "agent"},
		{http.MethodPost, "/v1/agent/client-chat", "agent"},
		{http.MethodPost, "/v1/chat", "chat"},
		{http.MethodPost, "/v1/chat/stream", "chat"},
		{http.MethodPost, "/v1/auth/login", "staff"},
		{http.MethodGet, "/v1/reports/jobs/abc", "staff"},
		{http.MethodPost, "/v1/push/subscriptions", "staff"},
		{http.MethodGet, "/v1/p/centrum/availability", "public"},
		{http.MethodHead, "/v1/p/centrum/facilities", "public"},
		{http.MethodPost, "/v1/p/centrum/records", "staff"},
		{http.MethodPatch, "/v1/p/centrum/records/abc", "staff"},
		{http.MethodGet, "/v1/sports/records", "other"},
		{http.MethodGet, "/static/app.js", ""},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		if got := SLOClass(req); got != c.want {
			t.Errorf("SLOClass(%s %s)=%q want %q", c.method, c.path, got, c.want)
		}
	}
}
