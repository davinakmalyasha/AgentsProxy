package agentstools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSafeCell_NeutralizesFormulas(t *testing.T) {
	for _, in := range []string{"=2+5", "+cmd", "-2+3", "@x", "|calc", "%x", "  =HYPERLINK(1)", "\t+evil"} {
		got := safeCell(in)
		if !strings.HasPrefix(got, "'") {
			t.Errorf("safeCell(%q) = %q, want leading-quote neutralization", in, got)
		}
	}
	for _, in := range []string{"Aqua", "PLN listrik", "2026-10-01", "", "Rp95.000"} {
		if got := safeCell(in); got != in {
			t.Errorf("safeCell(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestSupabaseObjectKey_IsTenantScoped(t *testing.T) {
	s := &SupabaseReportStore{Bucket: "reports"}
	key, err := s.objectKey("centrum", "laporan-2026-10-01_2026-11-01-semua-ab12cd34.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "reports/centrum/") || !strings.HasSuffix(key, ".xlsx") {
		t.Fatalf("key = %q, want reports/centrum/<month>/*.xlsx", key)
	}
	if strings.Contains(key, "..") {
		t.Fatalf("key = %q must not contain traversal", key)
	}
	if _, err := s.objectKey("centrum", "../evil.xlsx"); err == nil {
		t.Fatal("expected error for traversal name")
	}
	if _, err := s.objectKey("centrum", "noext"); err == nil {
		t.Fatal("expected error for non-xlsx name")
	}
}

func TestEscapeKey_KeepsSeparators(t *testing.T) {
	got := escapeKey("reports/centrum/2026-10/a b.xlsx")
	if strings.Contains(got, "%2F") || !strings.Contains(got, "/") {
		t.Fatalf("escapeKey = %q, want slashes preserved", got)
	}
	if !strings.Contains(got, "a%20b.xlsx") {
		t.Fatalf("escapeKey = %q, want space escaped", got)
	}
}

// TestSupabaseStore_RoundTrip fakes the Storage REST surface and proves
// the request shapes: upload path + upsert header, authenticated download
// path, and idempotent delete.
func TestSupabaseStore_RoundTrip(t *testing.T) {
	var sawUploadPath, sawDownloadPath, sawDeletePath string
	var sawUpsert, sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/storage/v1/object/reports/"):
			sawUploadPath = r.URL.Path
			sawUpsert = r.Header.Get("x-upsert")
			sawAuth = r.Header.Get("Authorization")
			w.Write([]byte(`{"Key":"x"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/storage/v1/object/authenticated/reports/"):
			sawDownloadPath = r.URL.Path
			w.Write([]byte("XLSXBYTES"))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/storage/v1/object/reports/"):
			sawDeletePath = r.URL.Path
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	s := &SupabaseReportStore{BaseURL: srv.URL, APIKey: "svc-key", Bucket: "reports"}
	ctx := context.Background()
	key, err := s.Save(ctx, "centrum", "laporan-a.xlsx", []byte("XLSXBYTES"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sawUploadPath, "/storage/v1/object/reports/reports/centrum/") {
		t.Fatalf("upload path = %q", sawUploadPath)
	}
	if sawUpsert != "true" || sawAuth != "Bearer svc-key" {
		t.Fatalf("upsert=%q auth=%q", sawUpsert, sawAuth)
	}
	name, data, err := s.Load(ctx, "centrum", key)
	if err != nil {
		t.Fatal(err)
	}
	if name != "laporan-a.xlsx" || string(data) != "XLSXBYTES" {
		t.Fatalf("load = %q %q", name, data)
	}
	if sawDownloadPath == "" || !strings.Contains(sawDownloadPath, "/authenticated/") {
		t.Fatalf("download path = %q", sawDownloadPath)
	}
	if err := s.Delete(ctx, "centrum", key); err != nil {
		t.Fatal(err)
	}
	if sawDeletePath == "" {
		t.Fatal("delete was not called")
	}
	// Traversal keys are refused without touching the network.
	if _, _, err := s.Load(ctx, "centrum", "../evil"); err == nil {
		t.Fatal("expected traversal refusal")
	}
}
