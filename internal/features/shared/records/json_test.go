package records

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMarkPublicCache(t *testing.T) {
	rec := httptest.NewRecorder()
	MarkPublicCache(rec, 10, 30)
	h := rec.Header()
	for _, key := range []string{"Vercel-CDN-Cache-Control", "CDN-Cache-Control"} {
		got := h.Get(key)
		if !strings.Contains(got, "s-maxage=10") || !strings.Contains(got, "stale-while-revalidate=30") {
			t.Errorf("%s = %q", key, got)
		}
		if strings.Contains(got, "private") || strings.Contains(got, "no-store") {
			t.Errorf("%s must be shareable at the CDN: %q", key, got)
		}
	}
	if got := h.Get("Cache-Control"); got != "" {
		t.Errorf("MarkPublicCache must not override browser Cache-Control; got %q", got)
	}
}

func TestMarkPublicCache_ClampsSWR(t *testing.T) {
	rec := httptest.NewRecorder()
	MarkPublicCache(rec, 10, 1)
	if got := rec.Header().Get("Vercel-CDN-Cache-Control"); !strings.Contains(got, "stale-while-revalidate=10") {
		t.Errorf("swr should not be below max-age: %q", got)
	}
}

func TestMarkPublicCache_DisabledForZero(t *testing.T) {
	rec := httptest.NewRecorder()
	MarkPublicCache(rec, 0, 30)
	if got := rec.Header().Get("Vercel-CDN-Cache-Control"); got != "" {
		t.Errorf("zero max-age must not mark the response cacheable: %q", got)
	}
}
