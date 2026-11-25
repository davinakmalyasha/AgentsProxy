package middleware

import (
	"net/http"
	"strings"
)

// addVary adds one token to the Vary header without duplicating it or
// dropping tokens another middleware already set (Origin, Accept-Encoding).
// A single canonical, comma-separated header is easier for CDNs to honor
// than repeated header lines.
func addVary(h http.Header, token string) {
	if token == "" {
		return
	}
	values := h.Values("Vary")
	if len(values) == 0 {
		h.Set("Vary", token)
		return
	}
	seen := false
	out := make([]string, 0, len(values)+1)
	for _, line := range values {
		for _, part := range strings.Split(line, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if strings.EqualFold(part, token) {
				seen = true
				continue
			}
			out = append(out, part)
		}
	}
	if !seen {
		out = append(out, token)
	}
	h.Set("Vary", strings.Join(out, ", "))
}

// removeVaryToken removes only the named token. The previous Gzip
// implementation deleted the whole Vary header on small/binary responses,
// silently erasing a CORS "Vary: Origin" that a shared cache depends on.
func removeVaryToken(h http.Header, token string) {
	if token == "" {
		return
	}
	values := h.Values("Vary")
	if len(values) == 0 {
		return
	}
	out := make([]string, 0, len(values))
	for _, line := range values {
		for _, part := range strings.Split(line, ",") {
			part = strings.TrimSpace(part)
			if part == "" || strings.EqualFold(part, token) {
				continue
			}
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		h.Del("Vary")
		return
	}
	h.Set("Vary", strings.Join(out, ", "))
}
