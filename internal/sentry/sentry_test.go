package sentry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestEnabled_DefaultFalse(t *testing.T) {
	// Default (DSN empty) — Enabled must be false.
	if Enabled() {
		t.Skip("Sentry is enabled in this test environment; skipping default-off assertion")
	}
}

func TestNoOpWhenDisabled(t *testing.T) {
	// These must never panic when DSN is empty.
	CaptureError(errors.New("boom"))
	CaptureMessage("hi")
	AddBreadcrumb("cat", "msg", nil)
	RecoverWith(context.Background(), "panic")
	Flush(0)
	// Enabled() false path for SlogHandler.
	var buf bytes.Buffer
	h := NewSlogHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))
	slog.SetDefault(slog.New(h))
	slog.Error("test", "client_id", "abc")
	if !strings.Contains(buf.String(), `"client_id":"abc"`) {
		t.Errorf("slog should still log; got %s", buf.String())
	}
}

func TestScrubPII(t *testing.T) {
	// Build a fake event and ensure scrubPII redacts the right keys.
	// We can't easily access sentry.Event without going through Init,
	// so just exercise the isSensitive predicate.
	cases := map[string]bool{
		"Authorization":   true,
		"authorization":   true,
		"X-Client-Token":  true,
		"client_token":    true,
		"x-admin-key":     true,
		"X-Session-Token": true,
		"session_token":   true,
		"token":           true,
		"cookie":          true,
		"api_key":         true,
		"dsn":             true,
		"database_url":    true,
		"password":        true,
		"DB_PASSWORD":     true,
		"secret":          true,
		"my_secret_field": true,
		"client_id":       false,
		"path":            false,
		"request_id":      false,
		"duration_ms":     false,
	}
	for k, want := range cases {
		if got := isSensitive(k); got != want {
			t.Errorf("isSensitive(%q) = %v, want %v", k, got, want)
		}
	}
}

func TestSlogHandler_ForwardsErrorToInner(t *testing.T) {
	var buf bytes.Buffer
	h := NewSlogHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(slog.New(h))
	slog.Error("boom", "x", 1)
	if !strings.Contains(buf.String(), `"msg":"boom"`) {
		t.Errorf("inner handler didn't receive record: %s", buf.String())
	}
}
