package api

import (
	"net/http"
	"testing"
)

func reqWith(remote, realIP, xff string) *http.Request {
	r, _ := http.NewRequest("GET", "http://x/", nil)
	r.RemoteAddr = remote
	if realIP != "" {
		r.Header.Set("X-Real-IP", realIP)
	}
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

func TestClientIPTrustedHonorsHeaders(t *testing.T) {
	// Loopback peer (local Next.js) → first XFF entry wins.
	r := reqWith("127.0.0.1:52341", "", "203.0.113.7, 10.0.0.1")
	if got := clientIP(r); got != "203.0.113.7" {
		t.Fatalf("got %q", got)
	}
	// X-Real-IP honored when no XFF.
	r = reqWith("[::1]:1234", "198.51.100.9", "")
	if got := clientIP(r); got != "198.51.100.9" {
		t.Fatalf("got %q", got)
	}
}

func TestClientIPVercelTrustsOverwrittenHeaders(t *testing.T) {
	// Vercel overwrites X-Forwarded-For at its edge; the function's TCP
	// peer is Vercel, not the browser. Trust the overwritten header so
	// rate-limit buckets are per visitor, not per Vercel proxy.
	t.Setenv("VERCEL", "1")
	r := reqWith("76.76.21.21:443", "", "203.0.113.7")
	if got := clientIP(r); got != "203.0.113.7" {
		t.Fatalf("got %q, want the overwritten client IP", got)
	}
}

func TestClientIPSpoofIgnored(t *testing.T) {
	// Untrusted direct peer: headers must NOT win.
	r := reqWith("203.0.113.99:443", "1.2.3.4", "5.6.7.8")
	if got := clientIP(r); got != "203.0.113.99" {
		t.Fatalf("spoofed: got %q", got)
	}
	// Garbage headers from trusted peer fall back to peer.
	r = reqWith("127.0.0.1:1111", "not-an-ip", "also-bad")
	if got := clientIP(r); got != "127.0.0.1" {
		t.Fatalf("got %q", got)
	}
}
