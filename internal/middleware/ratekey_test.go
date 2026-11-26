package middleware

import (
	"net/http"
	"testing"
)

// TestRateKey_IPOnlyBeforeAuth pins the security contract for the
// PRE-auth bucket key.
//
// This test previously asserted the opposite: it required
// rateKey() to hash X-Session-Token into a "t:<hash>" bucket, and
// required "different tokens → different buckets". That behaviour was
// the vulnerability. rateKey runs before any authentication
// middleware, so the header is unverified input chosen by the caller;
// deriving the bucket name from it let anyone mint a fresh bucket per
// request (`curl -H "X-Client-Token: $RANDOM"`) and bypass the limit
// on every route in the chain.
//
// Per-account isolation is preserved by verifiedClientKey (see
// RateLimitClient), which runs after the identity middleware has
// resolved a real client id.
func TestRateKey_IPOnlyBeforeAuth(t *testing.T) {
	newReq := func(addr string) *http.Request {
		r, _ := http.NewRequest("GET", "http://x/", nil)
		r.RemoteAddr = addr
		return r
	}

	t.Run("anonymous keys by IP", func(t *testing.T) {
		r := newReq("10.0.0.1:1234")
		if got := rateKey(r); got != "ip:10.0.0.1" {
			t.Fatalf("anon = %q want ip:10.0.0.1", got)
		}
	})

	t.Run("credential headers never change the pre-auth key", func(t *testing.T) {
		base := rateKey(newReq("10.0.0.1:1234"))

		for _, hdr := range []struct{ name, value string }{
			{"X-Session-Token", "sess-abc"},
			{"X-Client-Token", "client-xyz"},
			{"X-Session-Token", ""},
		} {
			r := newReq("10.0.0.1:1234")
			r.Header.Set(hdr.name, hdr.value)
			if got := rateKey(r); got != base {
				t.Errorf("%s=%q changed the pre-auth key to %q; it must stay %q",
					hdr.name, hdr.value, got, base)
			}
		}
	})

	t.Run("rotating a header cannot escape the IP bucket", func(t *testing.T) {
		seen := map[string]bool{}
		for i := 0; i < 50; i++ {
			r := newReq("10.0.0.1:1234")
			r.Header.Set("X-Client-Token", "token-"+string(rune('a'+i)))
			seen[rateKey(r)] = true
		}
		if len(seen) != 1 {
			t.Fatalf("50 distinct credentials produced %d buckets; want 1", len(seen))
		}
	})

	t.Run("X-Forwarded-For is ignored before trust is established", func(t *testing.T) {
		// internal/api/clientip.go owns forwarded-header handling and the
		// TRUSTED_PROXY_CIDRS check. Honouring it here would let a caller
		// pick their own bucket.
		base := rateKey(newReq("10.0.0.1:1234"))
		r := newReq("10.0.0.1:1234")
		r.Header.Set("X-Forwarded-For", "203.0.113.7")
		if got := rateKey(r); got != base {
			t.Errorf("X-Forwarded-For changed the key to %q; want %q", got, base)
		}
	})
}

// TestVerifiedClientKey covers the POST-auth key, which is where
// per-account isolation now lives.
func TestVerifiedClientKey(t *testing.T) {
	newReq := func(addr string) *http.Request {
		r, _ := http.NewRequest("GET", "http://x/", nil)
		r.RemoteAddr = addr
		return r
	}

	t.Run("resolved identity wins over IP", func(t *testing.T) {
		r := newReq("10.0.0.1:1234")
		r = r.WithContext(SetClientID(r.Context(), "client-1"))
		if got := verifiedClientKey(r); got != "c:client-1" {
			t.Fatalf("got %q want c:client-1", got)
		}
	})

	t.Run("two clients behind one IP get different keys", func(t *testing.T) {
		mk := func(id string) string {
			r := newReq("10.0.0.1:1234")
			return verifiedClientKey(r.WithContext(SetClientID(r.Context(), id)))
		}
		if mk("client-1") == mk("client-2") {
			t.Fatal("distinct clients must not share a bucket behind one NAT")
		}
	})

	t.Run("no identity yields no per-account key", func(t *testing.T) {
		// The route's pre-auth IP limiter is the bound; this limiter must
		// not open a duplicate IP-keyed bucket for the same cap.
		if got := verifiedClientKey(newReq("10.0.0.5:9999")); got != "" {
			t.Fatalf("got %q want empty (pre-auth limiter owns the IP bucket)", got)
		}
	})

	t.Run("no raw credential ever reaches a bucket key", func(t *testing.T) {
		r := newReq("10.0.0.1:1234")
		r.Header.Set("X-Session-Token", "super-secret-token")
		r = r.WithContext(SetClientID(r.Context(), "client-1"))
		if got := verifiedClientKey(r); got != "c:client-1" {
			t.Fatalf("key %q leaks or depends on the raw token", got)
		}
	})
}
