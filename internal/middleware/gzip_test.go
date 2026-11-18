package middleware

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestGzip_CompressesLargeJSON(t *testing.T) {
	big := `{"items":[` + strings.Repeat(`{"a":"0123456789abcdef"},`, 200) + `]}`
	h := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(big))
	}))
	req := httptest.NewRequest("GET", "/v1/sports/records", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	if res.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", res.Header.Get("Content-Encoding"))
	}
	gr, err := gzip.NewReader(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(gr)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != big {
		t.Error("round-trip body mismatch")
	}
}

func TestGzip_SmallPassesThrough(t *testing.T) {
	h := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	if res.Header.Get("Content-Encoding") == "gzip" {
		t.Error("small body should not be gzipped")
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q", body)
	}
}

func TestGzip_XlsxPassesThrough(t *testing.T) {
	big := bytes.Repeat([]byte{0x50, 0x4b, 0x03, 0x04}, 4096) // fake zip header bytes
	h := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
		_, _ = w.Write(big)
	}))
	req := httptest.NewRequest("GET", "/v1/reports/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	if res.Header.Get("Content-Encoding") == "gzip" {
		t.Error("xlsx must not be gzipped")
	}
	body, _ := io.ReadAll(res.Body)
	if !bytes.Equal(body, big) {
		t.Error("xlsx body altered")
	}
}

func TestGzip_BinaryHeaderAfterMiddlewareStripsLength(t *testing.T) {
	body := bytes.Repeat([]byte{0x25, 0x50, 0x44, 0x46, 0x2d, 0x31, 0x2e, 0x33, 0x0a}, 512) // %PDF-1.3
	h := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `attachment; filename="test.pdf"`)
		// Handler-set length arrives after Gzip stripped it at entry; it
		// must not truncate a compressed/passthrough wire body.
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	req := httptest.NewRequest("GET", "/api/v1/client/orders/x/invoice", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	if got := res.Header.Get("Content-Encoding"); got == "gzip" {
		t.Fatalf("pdf must pass through, got Content-Encoding %q", got)
	}
	if got := res.Header.Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length must be stripped, got %q", got)
	}
	got, _ := io.ReadAll(res.Body)
	if !bytes.Equal(got, body) {
		t.Fatal("pdf body altered")
	}
}

func TestGzip_NoAcceptEncoding(t *testing.T) {
	h := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 5000)))
	}))
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Result().Header.Get("Content-Encoding") == "gzip" {
		t.Error("must not gzip without Accept-Encoding")
	}
}
