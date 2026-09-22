// DONE: B3 (trailing-slash contract) / STANDARDS D
package middleware

import (
	"net/http"
	"strings"
)

// StripTrailingSlash rewrites (not redirects) "/x/" → "/x" before
// routing, for every method. Background: chi patterns are exact —
// r.Get("/products") matches /products but NOT /products/, while
// r.Post("/") under Route("/bookings") matches BOTH. The frontend
// connector always sends collection reads with a trailing slash and
// writes without, so without this middleware half the matrix 404s
// depending on each feature's registration style. Internal rewrite
// keeps POST bodies intact (a 301 redirect would drop them).
func StripTrailingSlash(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) > 1 && strings.HasSuffix(r.URL.Path, "/") {
			r2 := r.Clone(r.Context())
			r2.URL = r.URL.Clone()
			r2.URL.Path = strings.TrimSuffix(r.URL.Path, "/")
			r2.URL.RawPath = ""
			next.ServeHTTP(w, r2)
			return
		}
		next.ServeHTTP(w, r)
	})
}
