package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSetSessionCookiePolicy(t *testing.T) {
	const ttl = 30 * 24 * time.Hour

	dev := httptest.NewRecorder()
	setSessionCookie(dev, "dev-token", ttl, false)
	devCookie := dev.Result().Cookies()[0]
	if devCookie.Name != CookieName || devCookie.Value != "dev-token" {
		t.Fatalf("unexpected cookie %+v", devCookie)
	}
	if devCookie.MaxAge != int(ttl.Seconds()) {
		t.Fatalf("MaxAge = %d, want %d (30 days)", devCookie.MaxAge, int(ttl.Seconds()))
	}
	if devCookie.Secure {
		t.Fatal("development cookie must not require Secure")
	}
	if devCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("development SameSite = %v, want Strict", devCookie.SameSite)
	}

	prod := httptest.NewRecorder()
	setSessionCookie(prod, "prod-token", ttl, true)
	prodCookie := prod.Result().Cookies()[0]
	if !prodCookie.Secure {
		t.Fatal("production cookie must be Secure")
	}
	if prodCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("production SameSite = %v, want Strict", prodCookie.SameSite)
	}
	if prodCookie.MaxAge != 2592000 {
		t.Fatalf("production MaxAge = %d, want 2592000", prodCookie.MaxAge)
	}
}
