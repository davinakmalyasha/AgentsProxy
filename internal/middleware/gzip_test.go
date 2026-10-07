package middleware

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
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
