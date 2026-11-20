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
	trustedMu   sync.RWMutex
	trustedNets []netip.Prefix
	configured  bool
)

func defaultTrusted() []netip.Prefix {
	return []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("::1/128"),
	}
}

func parseTrustedCIDRs(cidrs []string) []netip.Prefix {
	nets := defaultTrusted()
	for _, part := range cidrs {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			part += "/32"
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			nets = append(nets, p)
		}
	}
	return nets
}

// ConfigureTrustedProxies installs the trusted-proxy set from config.
// Called once at boot from cfg.TrustedProxyCIDRs; invalid entries are
// skipped (fail-closed: an unparsable CIDR trusts nothing new).
// Unconfigured processes keep loopback-only trust.
func ConfigureTrustedProxies(cidrs []string) {
	trustedMu.Lock()
	defer trustedMu.Unlock()
	trustedNets = parseTrustedCIDRs(cidrs)
	configured = true
}

func currentTrusted() []netip.Prefix {
	trustedMu.RLock()
	defer trustedMu.RUnlock()
	if !configured {
		return defaultTrusted()
	}
	return trustedNets
}

// trustedPeer reports whether the TCP peer may set forwarding headers.
func onVercel() bool {
	return strings.TrimSpace(os.Getenv("VERCEL")) == "1"
}

func trustedPeer(remoteAddr string) bool {
	// Vercel overwrites X-Forwarded-For and does not forward an
	// upstream-supplied value, so its proxy is the trust boundary even
	// though its egress addresses are dynamic. VERCEL=1 is injected by
	// the platform; a direct, non-Vercel deployment never sees it.
	if onVercel() {
		return true
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return false
	}
	for _, p := range currentTrusted() {
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
		// Vercel documents X-Forwarded-For as overwritten and identical to
		// X-Real-IP. Prefer it so a client-supplied X-Real-IP cannot win.
		if onVercel() {
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				if ip := cleanIP(strings.Split(xff, ",")[0]); ip != "" {
					return ip
				}
			}
		}
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
