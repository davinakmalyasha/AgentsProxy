// DONE: PRD 7 (panic -> 500 + Sentry) / STANDARDS A, D, H
package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/davinakmalyasha/agents-proxy/internal/sentry"
)

// DONE: PRD §7 (server hardening) / STANDARDS §A, §D, §H (slog, recovery, Sentry env-gated)
func Recovery() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					stack := string(debug.Stack())
					slog.Error("panic recovered",
						"err", fmt.Sprint(rec),
						"path", r.URL.Path,
						"method", r.Method,
						"stack", stack,
					)
					// Forward to Sentry if configured (no-op when DSN empty).
					if sentry.Enabled() {
						sentry.RecoverWith(r.Context(), rec)
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"error":"internal"}`))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
