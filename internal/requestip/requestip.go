// Package requestip resolves the caller IP with one trust boundary for
// every surface: platform rate limiting, public chat history, agency
// rate limiting, and audit rows.
//
// X-Real-IP / X-Forwarded-For are honored only when the TCP peer is a
// trusted proxy. Vercel overwrites X-Forwarded-For at its edge, so
// VERCEL=1 is itself a trust boundary. Everywhere else the socket peer
// wins, which means a direct client cannot choose its own rate-limit
// bucket or forge an audit IP by sending headers.
package requestip

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

// ConfigureTrustedProxies installs the trusted-proxy set. Invalid entries
// are skipped (fail closed: an unparsable CIDR trusts nothing new).
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

func onVercel() bool {
	return strings.TrimSpace(os.Getenv("VERCEL")) == "1"
}

// TrustedPeer reports whether the TCP peer may set forwarding headers.
func TrustedPeer(remoteAddr string) bool {
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

func cleanIP(s string) string {
	ip, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil || !ip.IsValid() {
		return ""
	}
	return ip.String()
}

// ClientIP returns the caller IP: forwarding headers only from trusted
// peers, else the TCP peer with the port stripped. It never returns an
// empty string, so Redis keys stay well-formed.
func ClientIP(r *http.Request) string {
	if TrustedPeer(r.RemoteAddr) {
		// Vercel documents X-Forwarded-For as overwritten and identical
		// to X-Real-IP. Prefer it so a client-supplied X-Real-IP cannot
		// win there.
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
