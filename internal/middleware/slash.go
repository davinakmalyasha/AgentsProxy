// DONE: B3 (trailing-slash contract) / STANDARDS D
package middleware

import (
	"net/http"
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
		// Mutate in place: the server owns the Request, so the two
		// Clone calls (request + URL, ~6 allocs) buy nothing.
		if p := r.URL.Path; len(p) > 1 && p[len(p)-1] == '/' {
			r.URL.Path = p[:len(p)-1]
			r.URL.RawPath = ""
		}
		next.ServeHTTP(w, r)
	})
}
