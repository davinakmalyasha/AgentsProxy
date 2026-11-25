package records

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// writeJSON serializes v as JSON with the given status. Panics on
// marshal error (it would mean our own code is broken, not user
// input).
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// MarkPublicCache marks a successful landing-page read as shareable at the
// CDN for maxAge seconds. It deliberately does not touch Cache-Control:
// SecurityHeaders keeps browser/proxy caching at no-store, while Vercel's
// CDN honors the more specific header. Only public GET 200 responses may
// call this; authenticated responses must never carry it.
func MarkPublicCache(w http.ResponseWriter, maxAge, staleWhileRevalidate int) {
	if maxAge <= 0 {
		return
	}
	if staleWhileRevalidate < maxAge {
		staleWhileRevalidate = maxAge
	}
	value := fmt.Sprintf("public, s-maxage=%d, stale-while-revalidate=%d", maxAge, staleWhileRevalidate)
	h := w.Header()
	h.Set("Vercel-CDN-Cache-Control", value)
	h.Set("CDN-Cache-Control", value)
}

// emptyPayloadJSON is the shared empty-object payload. List handlers
// stamp it on rows with NULL/empty payloads instead of allocating one
// []byte("{}") per row; it is read-only after init (marshaled, never
// mutated).
var emptyPayloadJSON = []byte("{}")

// detachedCtx returns a context for post-commit bookkeeping (metering):
// detached from client cancellation so a disconnect after the response
// is computed cannot drop the row, bounded to 5s so a hung DB cannot
// hold the goroutine forever. Callers defer the cancel.
func detachedCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}
