package middleware

import (
	"net/http"
	"strings"
	"testing"
)

func TestRateKeyCredentialAware(t *testing.T) {
	r, _ := http.NewRequest("GET", "http://x/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	if got := rateKey(r); got != "ip:10.0.0.1" {
		t.Fatalf("anon = %q", got)
	}
	r.Header.Set("X-Session-Token", "sess-abc")
	k1 := rateKey(r)
	if !strings.HasPrefix(k1, "t:") || len(k1) != 18 {
		t.Fatalf("session key = %q", k1)
	}
	// Same token → same bucket (stable); raw token never in key.
	r2, _ := http.NewRequest("GET", "http://x/", nil)
	r2.RemoteAddr = "10.9.9.9:9999"
	r2.Header.Set("X-Session-Token", "sess-abc")
	if rateKey(r2) != k1 {
		t.Fatal("same token must map to same bucket across IPs")
	}
	if strings.Contains(k1, "sess-abc") {
		t.Fatal("raw token leaked into bucket key")
	}
	// Different tokens → different buckets.
	r.Header.Set("X-Session-Token", "sess-other")
	if rateKey(r) == k1 {
		t.Fatal("different tokens must not share a bucket")
	}
}
