// Caller-IP resolution with a trust boundary (B3.4).
//
// X-Real-IP / X-Forwarded-For are only honored when the TCP peer is a
// trusted proxy (loopback by default for the local Next.js server;
// extend via TRUSTED_PROXY_CIDRS="cidr,ip,..." in prod where the
// frontend runs on Vercel). Direct clients can no longer spoof another
// visitor's memory key (ag:) or quota bucket (rpd:) via headers.
//
// DONE: B3 (IP spoof hardening)
package api

import (
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
)

var (
	trustedOnce sync.Once
	trustedNets []netip.Prefix
)

func loadTrustedProxies() {
	trustedNets = []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("::1/128"),
	}
	for _, part := range strings.Split(os.Getenv("TRUSTED_PROXY_CIDRS"), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			part += "/32"
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			trustedNets = append(trustedNets, p)
		}
	}
}

// trustedPeer reports whether the TCP peer may set forwarding headers.
func trustedPeer(remoteAddr string) bool {
	trustedOnce.Do(loadTrustedProxies)
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return false
	}
	for _, p := range trustedNets {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// cleanIP validates a header-supplied IP (no ports, no garbage).
func cleanIP(s string) string {
	ip, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil || !ip.IsValid() {
		return ""
	}
	return ip.String()
}

// clientIP extracts the caller IP: forwarding headers only from trusted
// peers, else the TCP peer address with port stripped. Never empty —
// falls back to "unknown" so Redis keys stay well-formed.
func clientIP(r *http.Request) string {
	if trustedPeer(r.RemoteAddr) {
		if ip := cleanIP(r.Header.Get("X-Real-IP")); ip != "" {
			return ip
		}
		// Standard: FIRST entry is the original client.
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if ip := cleanIP(strings.Split(xff, ",")[0]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := cleanIP(host); ip != "" {
		return ip
	}
	return "unknown"
}
