package records

import (
	"context"
	"encoding/json"
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

// emptyPayloadJSON is the shared empty-object payload. List handlers
// stamp it on rows with NULL/empty payloads instead of allocating one
// []byte("{}") per row; it is read-only after init (marshaled, never
// mutated).
var emptyPayloadJSON = []byte("{}")

// detachedCtx returns a context for post-commit bookkeeping (metering):
// detached from client cancellation so a disconnect after the response
// is computed cannot drop the row, bounded to 5s so a hung DB cannot
// hold the goroutine forever. The timer fires and releases itself;
// no cancel func is returned by design (fire-and-forget scope).
func detachedCtx(ctx context.Context) context.Context {
	c, _ := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	return c
}
