// DONE: PRD H (Sentry env-gated, no-op when DSN empty, PII redaction, panic capture, slog bridge) / STANDARDS H
// Package sentry wraps the official sentry-go SDK so the rest of the
// codebase has one short import path and one place to enforce PII
// redaction. When SENTRY_DSN is empty every public function is a
// no-op — the proxy runs identically with or without Sentry.
//
// DONE: PRD §H (Sentry env-gated) / STANDARDS §H
package sentry

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
)

var (
	enabled bool
	mu      sync.RWMutex
)

// Options configures the Sentry client.
type Options struct {
	DSN              string
	Environment      string
	Release          string
	TracesSampleRate float64
	ServerName       string
}

// Init configures the global Sentry client. If DSN is empty, Init is
// a no-op and Enabled() returns false. Callers must call Flush before
// process exit.
func Init(o Options) {
	if strings.TrimSpace(o.DSN) == "" {
		slog.Info("sentry disabled (SENTRY_DSN empty)")
		return
	}
	env := o.Environment
	if env == "" {
		env = "development"
	}
	release := o.Release
	if release == "" {
		release = "dev"
	}
	traces := o.TracesSampleRate
	// 0 is a valid explicit choice (disables tracing); only negative
	// values are clamped, and config already rejects those.
	if traces < 0 {
		traces = 0.1
	}
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              o.DSN,
		Environment:      env,
		Release:          release,
		ServerName:       o.ServerName,
		TracesSampleRate: traces,
		AttachStacktrace: true,
		BeforeSend: func(e *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			scrubPII(e)
			return e
		},
		BeforeSendTransaction: func(e *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			scrubPII(e)
			return e
		},
	})
	if err != nil {
		slog.Error("sentry init", "err", err)
		return
	}
	mu.Lock()
	enabled = true
	mu.Unlock()
	slog.Info("sentry initialised", "environment", env, "release", release, "traces_sample_rate", traces)
}

// Enabled reports whether Sentry is active.
func Enabled() bool {
	mu.RLock()
	defer mu.RUnlock()
	return enabled
}

// Flush waits up to d for queued events to send. No-op when disabled.
func Flush(d time.Duration) {
	if !Enabled() {
		return
	}
	sentry.Flush(d)
}

// CaptureMessage sends a message event. No-op when disabled.
func CaptureMessage(msg string) {
	if !Enabled() {
		return
	}
	sentry.CaptureMessage(msg)
}

// AddBreadcrumb records a breadcrumb on the current scope. No-op when
// disabled. Safe to call from any goroutine.
func AddBreadcrumb(category, message string, data map[string]any) {
	if !Enabled() {
		return
	}
	sentry.AddBreadcrumb(&sentry.Breadcrumb{
		Category: category,
		Message:  message,
		Data:     data,
		Level:    sentry.LevelInfo,
	})
}

// RecoverWith forwards a recovered panic on the given context to
// Sentry. No-op when DSN empty.
func RecoverWith(ctx context.Context, rec interface{}) {
	if !Enabled() {
		return
	}
	sentry.CurrentHub().RecoverWithContext(ctx, rec)
	sentry.Flush(2 * time.Second)
}

// SlogHandler wraps an inner slog handler and forwards Error-level
// records to Sentry. INFO/WARN/DEBUG go to the inner handler only.
type SlogHandler struct {
	inner slog.Handler
}

// NewSlogHandler returns a slog handler that delegates to inner and
// also pipes Error records to Sentry. If Sentry is disabled, it
// behaves identically to inner.
func NewSlogHandler(inner slog.Handler) *SlogHandler { return &SlogHandler{inner: inner} }

func (h *SlogHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.inner.Enabled(ctx, l) }
func (h *SlogHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &SlogHandler{inner: h.inner.WithAttrs(as)}
}
func (h *SlogHandler) WithGroup(name string) slog.Handler {
	return &SlogHandler{inner: h.inner.WithGroup(name)}
}
func (h *SlogHandler) Handle(ctx context.Context, r slog.Record) error {
	if err := h.inner.Handle(ctx, r); err != nil {
		return err
	}
	if Enabled() && r.Level >= slog.LevelError {
		extra := map[string]any{}
		r.Attrs(func(a slog.Attr) bool {
			extra[redactKey(a.Key)] = a.Value.Any()
			return true
		})
		sentry.CaptureException(&slogRecordError{record: r, extra: extra})
	}
	return nil
}

type slogRecordError struct {
	record slog.Record
	extra  map[string]any
}

func (e *slogRecordError) Error() string { return e.record.Message }

// scrubPII removes auth tokens, session tokens, cookies, and secret-ish
// values from a Sentry event before transmission.
func scrubPII(e *sentry.Event) {
	if e == nil {
		return
	}
	for k := range e.Tags {
		if isSensitive(k) {
			e.Tags[k] = "[REDACTED]"
		}
	}
	// Drop any Contexts key that looks sensitive. sentry.Context is
	// a struct (not a free-form map) so we can't mutate the value
	// in place; deleting the key is the safe path.
	for k := range e.Contexts {
		if isSensitive(k) {
			delete(e.Contexts, k)
		}
	}
	if e.Request != nil {
		if e.Request.Headers != nil {
			for k := range e.Request.Headers {
				if isSensitive(k) {
					e.Request.Headers[k] = "[REDACTED]"
				}
			}
		}
		if e.Request.Cookies != "" {
			e.Request.Cookies = "[REDACTED]"
		}
	}
}

// IsSensitive reports whether a log/event key carries a secret value.
// Exported so the server's slog redactor and this package agree on one
// list instead of drifting into two.
func IsSensitive(k string) bool {
	return isSensitive(k)
}

func isSensitive(k string) bool {
	kl := strings.ToLower(k)
	switch kl {
	case "authorization", "x-client-token", "client_token", "x-admin-key",
		"x-session-token", "session_token", "cookie", "set-cookie",
		"api_key", "apikey", "dsn", "database_url", "token":
		return true
	}
	return strings.Contains(kl, "password") || strings.Contains(kl, "secret")
}

func redactKey(k string) string {
	if isSensitive(k) {
		return "[REDACTED]"
	}
	return k
}
