package requestip

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(remote, realIP, xff string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remote
	if realIP != "" {
		r.Header.Set("X-Real-IP", realIP)
	}
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

func TestClientIPTrustedLoopback(t *testing.T) {
	t.Setenv("VERCEL", "")
	r := req("127.0.0.1:52341", "", "203.0.113.7, 10.0.0.1")
	if got := ClientIP(r); got != "203.0.113.7" {
		t.Fatalf("ClientIP = %q, want first forwarded client", got)
	}
}

func TestClientIPConfiguredProxy(t *testing.T) {
	t.Setenv("VERCEL", "")
	ConfigureTrustedProxies([]string{"10.0.0.0/8"})
	t.Cleanup(func() { ConfigureTrustedProxies(nil) })
	if got := ClientIP(req("10.1.2.3:443", "", "203.0.113.8")); got != "203.0.113.8" {
		t.Fatalf("ClientIP = %q, want forwarded IP behind configured proxy", got)
	}
}

func TestClientIPVercel(t *testing.T) {
	t.Setenv("VERCEL", "1")
	if got := ClientIP(req("76.76.21.21:443", "", "203.0.113.9")); got != "203.0.113.9" {
		t.Fatalf("ClientIP = %q, want Vercel forwarded client", got)
	}
}

func TestClientIPSpoofIgnored(t *testing.T) {
	t.Setenv("VERCEL", "")
	ConfigureTrustedProxies(nil)
	r := req("203.0.113.99:443", "1.2.3.4", "5.6.7.8")
	if got := ClientIP(r); got != "203.0.113.99" {
		t.Fatalf("ClientIP = %q, spoofed headers won", got)
	}
}
