// DONE: PRD 7 (slog + request_id + Sentry breadcrumb) / STANDARDS A, B, H
package middleware

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
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

// InstanceID is a process-boot identifier. Vercel/containers reuse a warm
// process across requests, so this is the honest cold/warm grouping key; the
// request ID is not an instance ID. It is exposed as a response header for
// the benchmark harness and never includes host or tenant data.
func InstanceID() string {
	return reqPrefix()
}

// DONE: PRD §7 (request_id + slog) / STANDARDS §A, §B, §H
func RequestID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(HeaderRequestID)
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
	status  int
	bytes   int
	started time.Time
}

func (s *statusRecorder) finalizeHeaders() {
	if s.Header().Get("Server-Timing") == "" {
		ms := float64(time.Since(s.started).Microseconds()) / 1000.0
		s.Header().Set("Server-Timing", fmt.Sprintf("app;dur=%.3f", ms))
	}
}

func (s *statusRecorder) WriteHeader(code int) {
	s.finalizeHeaders()
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.finalizeHeaders()
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
			w.Header().Set("X-Instance-Id", InstanceID())
			rec := &statusRecorder{ResponseWriter: w, started: start}
			next.ServeHTTP(rec, r)
			dur := time.Since(start)
			cid, _ := r.Context().Value(CtxClientID).(string)
			rid, _ := r.Context().Value(CtxRequestID).(string)
			attrs := make([]any, 0, 12)
			attrs = append(attrs,
				"method", r.Method,
				"path", RedactPath(r.URL.Path),
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
					"path":        RedactPath(r.URL.Path),
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

// RedactPath removes capability credentials from request paths before they
// reach logs, Sentry breadcrumbs, or panic reports. Report downloads and
// report-job links authenticate by possession of the path segment itself, so
// logging the raw path is equivalent to logging a password.
func RedactPath(path string) string {
	switch {
	case strings.HasPrefix(path, "/v1/reports/jobs/"):
		return "/v1/reports/jobs/[REDACTED]"
	case strings.HasPrefix(path, "/v1/reports/"):
		return "/v1/reports/[REDACTED]"
	default:
		return path
	}
}

// Fingerprint is a short, stable label for a capability value that must be
// correlated in logs without being usable. It is not a replacement for
// RedactPath; use it when an opaque id (not a full path) needs a log field.
func Fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:4])
}
