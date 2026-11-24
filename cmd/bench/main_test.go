package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestBenchHealthAndPublic(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/v1/p/centrum/facilities", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	outPath := filepath.Join(t.TempDir(), "results.json")
	var out, errOut bytes.Buffer
	code := run([]string{
		"-base-url", srv.URL,
		"-duration", "200ms",
		"-warmup", "0s",
		"-concurrency", "2",
		"-scenarios", "health,public-facilities",
		"-client-token", "x",
		"-staff-token", "x",
		"-json", outPath,
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("run=%d stderr=%s out=%s", code, errOut.String(), out.String())
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var results []stats
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	for _, r := range results {
		if r.Requests == 0 || r.Errors != 0 {
			t.Fatalf("%s requests=%d errors=%d", r.Name, r.Requests, r.Errors)
		}
	}
}

func TestBenchCountsUnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	var out, errOut bytes.Buffer
	code := run([]string{
		"-base-url", srv.URL,
		"-duration", "100ms",
		"-warmup", "0s",
		"-concurrency", "1",
		"-scenarios", "health",
		"-staff-token", "x",
	}, &out, &errOut)
	if code != 1 {
		t.Fatalf("run=%d, want 1; out=%s", code, out.String())
	}
}
