// DONE: PRD 7 (slog + request_id + Sentry breadcrumb) / STANDARDS A, B, H
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/davinakmalyasha/agents-proxy/internal/sentry"
)

type ctxKey int

const (
	CtxRequestID ctxKey = iota
	CtxClientID
)

const HeaderRequestID = "X-Request-ID"

// DONE: PRD §7 (request_id + slog) / STANDARDS §A, §B, §H
func RequestID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(HeaderRequestID)
			if id == "" {
				b := make([]byte, 8)
				_, _ = rand.Read(b)
				id = hex.EncodeToString(b)
			}
			w.Header().Set(HeaderRequestID, id)
			ctx := context.WithValue(r.Context(), CtxRequestID, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

func Logging() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			dur := time.Since(start)
			cid, _ := r.Context().Value(CtxClientID).(string)
			rid, _ := r.Context().Value(CtxRequestID).(string)
			attrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", dur.Milliseconds(),
				"request_id", rid,
			}
			if cid != "" {
				attrs = append(attrs, "client_id", cid)
			}
			slog.Info("http", attrs...)
			// Sentry breadcrumb so events in this request carry context.
			if sentry.Enabled() {
				bc := map[string]any{
					"method":      r.Method,
					"path":        r.URL.Path,
					"status":      rec.status,
					"duration_ms": dur.Milliseconds(),
					"request_id":  rid,
				}
				if cid != "" {
					bc["client_id"] = cid
				}
				sentry.AddBreadcrumb("http", "request", bc)
			}
		})
	}
}

func SetClientID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, CtxClientID, id)
}
