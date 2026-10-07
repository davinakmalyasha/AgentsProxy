// DONE: PRD 7 (slog + request_id + Sentry breadcrumb) / STANDARDS A, B, H
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/davinakmalyasha/agents-proxy/internal/sentry"
)

type ctxKey int

const (
	CtxRequestID ctxKey = iota
	CtxClientID
)

const HeaderRequestID = "X-Request-ID"

// reqIDPrefix is process-unique (random at startup); the counter makes
// IDs unique within the process. Zero per-request syscalls, unlike
// crypto/rand per request — and on Vercel the platform ID wins anyway.
var (
	reqIDPrefix  string
	reqIDCounter atomic.Uint64
	reqIDOnce    sync.Once
)

func reqPrefix() string {
	reqIDOnce.Do(func() {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		reqIDPrefix = hex.EncodeToString(b)
	})
	return reqIDPrefix
}

// DONE: PRD §7 (request_id + slog) / STANDARDS §A, §B, §H
func RequestID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(HeaderRequestID)
			if id == "" {
				// Prefer the platform's own ID (Vercel sets one per
				// invocation) over minting: correlation survives the
				// proxy hops instead of forking a second ID space.
				id = r.Header.Get("X-Vercel-Id")
			}
			if id == "" {
				id = fmt.Sprintf("%s-%d", reqPrefix(), reqIDCounter.Add(1))
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

// ReadFrom preserves io.ReaderFrom through the wrapper: without it,
// io.Copy (http.ServeContent for .xlsx downloads) falls back to a
// 32KB-buffer copy loop instead of the fast path.
func (s *statusRecorder) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := s.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(r)
		s.bytes += int(n)
		return n, err
	}
	return io.Copy(struct{ io.Writer }{s}, r)
}

// Flush implements http.Flusher so a future SSE/streaming handler does
// not silently buffer behind this middleware.
func (s *statusRecorder) Flush() {
	if s.status == 0 {
		s.WriteHeader(http.StatusOK)
	}
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer for http.ResponseController
// (Go 1.20+): without it, ResponseController cannot find Flush,
// Hijack, or ReadFrom through the wrapper.
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

func Logging() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			dur := time.Since(start)
			cid, _ := r.Context().Value(CtxClientID).(string)
			rid, _ := r.Context().Value(CtxRequestID).(string)
			attrs := make([]any, 0, 12)
			attrs = append(attrs,
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", dur.Milliseconds(),
				"request_id", rid,
			)
			if cid != "" {
				attrs = append(attrs, "client_id", cid)
			}
			// Successful reads are Debug: at Info, every landing-page
			// poll and every /healthz probe is a log line, and on
			// Vercel log volume is a metered ingestion path. Errors
			// and writes stay Info so failures are always visible.
			if rec.status >= 400 || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
				slog.Info("http", attrs...)
			} else {
				slog.Debug("http", attrs...)
			}
			// Sentry breadcrumb so events in this request carry context.
			// Skip liveness probes: they would flood breadcrumbs with
			// zero user value.
			if r.URL.Path != "/healthz" && sentry.Enabled() {
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
