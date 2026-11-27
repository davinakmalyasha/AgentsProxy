// Package pushsafe owns the outbound Web Push trust boundary.
//
// A push subscription is user-supplied data: the browser sends an endpoint
// URL, we store it, and a background worker later makes an HTTP request to
// it. Without a host allowlist that is a server-side request forgery
// primitive: any authenticated staff account could register an internal
// address (metadata service, loopback admin port) and have the scheduler
// probe it for them.
//
// Web Push endpoints are issued by a small, stable set of browser push
// services. Only those hosts (and their documented subdomains) are valid.
// Validation runs on registration and again immediately before every send,
// so old rows written before this policy cannot be replayed into an SSRF.
package pushsafe

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/davinakmalyasha/agents-proxy/internal/httpx"
)

// exactHosts are the endpoint hosts used by the major push services.
// Keep this list deliberately small: a new browser vendor is a code
// change, not a configuration value that an attacker could influence.
var exactHosts = map[string]struct{}{
	"fcm.googleapis.com":                {}, // Chrome, Edge (Chromium), Brave, Opera
	"web.push.apple.com":                {}, // Safari
	"push.services.mozilla.com":         {}, // Firefox (legacy/alternate)
	"updates.push.services.mozilla.com": {}, // Firefox
}

// suffixHosts are provider-operated subdomains. The dot boundary is
// required: "evilnotify.windows.com" must not match.
var suffixHosts = []string{
	".notify.windows.com", // Edge / Windows Notification Service
}

func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

// AllowedHost reports whether host is an allowed Web Push service.
func AllowedHost(host string) bool {
	h := normalizeHost(host)
	if h == "" {
		return false
	}
	if _, ok := exactHosts[h]; ok {
		return true
	}
	for _, suffix := range suffixHosts {
		if len(h) > len(suffix) && strings.HasSuffix(h, suffix) {
			return true
		}
	}
	return false
}

// ValidateEndpoint accepts only HTTPS URLs on an allowed push-service host,
// with no userinfo and the default TLS port. It returns an error suitable
// for an internal log; API responses should stay generic.
func ValidateEndpoint(raw string) error {
	if raw == "" {
		return errors.New("empty endpoint")
	}
	if len(raw) > 2048 {
		return errors.New("endpoint too long")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse endpoint: %w", err)
	}
	if u.Scheme != "https" {
		return errors.New("push endpoint must use https")
	}
	if u.User != nil {
		return errors.New("push endpoint must not contain userinfo")
	}
	if u.Host == "" || u.Hostname() == "" {
		return errors.New("push endpoint has no host")
	}
	if port := u.Port(); port != "" && port != "443" {
		return errors.New("push endpoint must use port 443")
	}
	if !AllowedHost(u.Hostname()) {
		return fmt.Errorf("push endpoint host %q is not an allowed push service", u.Hostname())
	}
	if u.Path == "" || u.Path == "/" {
		return errors.New("push endpoint has no path")
	}
	return nil
}

// Host returns the normalized host of an endpoint for safe logging. It
// returns "" when the value cannot be parsed, which is also safe to log.
func Host(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return normalizeHost(u.Hostname())
}

// NewHTTPClient returns an HTTP client that refuses redirects to anything
// outside the push-service allowlist. webpush-go follows redirects by
// default; without this check an allowlisted endpoint could 302 the worker
// to an internal address.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: httpx.SharedTransport(),
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many push endpoint redirects")
			}
			if req.URL.Scheme != "https" || !AllowedHost(req.URL.Hostname()) {
				return errors.New("push endpoint redirected outside the allowlist")
			}
			return nil
		},
	}
}
