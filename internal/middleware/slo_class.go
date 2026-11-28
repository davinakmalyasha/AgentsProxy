package middleware

import (
	"net/http"
	"strings"
)

// SLOClass maps a request to the aggregate class used by internal/slo.
//
// Classification is by surface, not by credential, because this runs before
// identity is resolved. That is deliberate: the question the SLO answers is
// "how fast is the staff/project surface" not "who was this user". A GET under
// /v1/p/ is the public landing surface; a write there requires staff/client
// credentials. /healthz is excluded (probe traffic would drown the signal) and
// non-API paths return "".
func SLOClass(r *http.Request) string {
	p := r.URL.Path
	switch {
	case p == "/healthz" || strings.HasPrefix(p, "/healthz/"):
		return ""
	case strings.HasPrefix(p, "/v1/admin/"), p == "/v1/admin":
		return "admin"
	case strings.HasPrefix(p, "/v1/agent/"):
		return "agent"
	case p == "/v1/chat" || p == "/v1/chat/stream":
		return "chat"
	case strings.HasPrefix(p, "/v1/auth/"):
		return "staff"
	case strings.HasPrefix(p, "/v1/reports/"):
		return "staff"
	case strings.HasPrefix(p, "/v1/push/"):
		return "staff"
	case strings.HasPrefix(p, "/v1/p/"):
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			return "public"
		}
		return "staff"
	case strings.HasPrefix(p, "/v1/"):
		return "other"
	}
	return ""
}
