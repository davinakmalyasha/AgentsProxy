package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func smokeServer(t *testing.T, quarantined bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "version": "abc123", "db": true, "redis": true,
		})
	})
	for _, path := range []string{"/v1/p/centrum/availability", "/v1/p/centrum/pricing"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[]}`))
		})
	}
	mux.HandleFunc("/v1/p/centrum/facilities", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Origin", "https://centrum-badminton.vercel.app")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	mux.HandleFunc("/v1/p/centrum/records", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"missing X-Client-Token"}`))
	})
	mux.HandleFunc("/v1/sports/facilities", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	})
	mux.HandleFunc("/v1/admin/tenant-health", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer admin" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		q := map[string]any{}
		if quarantined {
			q["centrum"] = map[string]any{"Quarantined": true, "Reason": "empty entitlement"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"quarantined": q})
	})
	return httptest.NewServer(mux)
}

func runSmoke(t *testing.T, srv *httptest.Server, extra ...string) (int, string) {
	t.Helper()
	args := []string{
		"-base-url", srv.URL,
		"-attempts", "1",
		"-retry-delay", "0s",
		"-expect-version", "abc123",
		"-admin-key", "admin",
		"-origin", "https://centrum-badminton.vercel.app",
	}
	args = append(args, extra...)
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String()
}

func TestRunPassesContract(t *testing.T) {
	srv := smokeServer(t, false)
	defer srv.Close()
	code, out := runSmoke(t, srv)
	if code != 0 {
		t.Fatalf("run=%d output:\n%s", code, out)
	}
	for _, want := range []string{"PASS health", "PASS facilities", "PASS pricing", "PASS records-unauthenticated", "PASS cors-preflight", "PASS tenant-health"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunFailsOnVersionMismatch(t *testing.T) {
	srv := smokeServer(t, false)
	defer srv.Close()
	code, out := runSmoke(t, srv, "-expect-version", "different")
	if code != 1 {
		t.Fatalf("run=%d, want 1; output:\n%s", code, out)
	}
	if !strings.Contains(out, "FAIL health") {
		t.Errorf("output missing health failure:\n%s", out)
	}
}

func TestRunFailsOnQuarantinedTenant(t *testing.T) {
	srv := smokeServer(t, true)
	defer srv.Close()
	code, out := runSmoke(t, srv)
	if code != 1 {
		t.Fatalf("run=%d, want 1; output:\n%s", code, out)
	}
	if !strings.Contains(out, "FAIL tenant-health") {
		t.Errorf("output missing tenant-health failure:\n%s", out)
	}
}
