// SLO reporting: server-observed latency percentiles derived from real
// traffic, not from a synthetic tenant. Admin-only: the numbers are aggregate
// but still operational detail (class volumes and error counts).
package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/slo"
)

type SLOReporter struct {
	RDB *redis.Client
}

func (h SLOReporter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Boot normally configures this already; the fallback keeps the endpoint
	// usable in tests and on hosts with a different boot order.
	slo.EnsureConfigured(h.RDB, "slo")

	window := 60
	if v := r.URL.Query().Get("window"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			window = n
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rep, err := slo.BuildReport(ctx, window)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "slo unavailable")
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
