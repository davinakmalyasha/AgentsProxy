// Package testsafety guards destructive DB/Redis tests.
//
// The suite deletes clients, drops tenant schemas, and calls FlushDB. A
// wrapper script is not enough: a direct `go test` run must also refuse a
// remote target. This package is imported only by tests and test helpers.
package testsafety

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
)

// RequireLocal fails the test before destructive setup when any configured
// test target is not loopback.
func RequireLocal(t testing.TB) {
	t.Helper()
	if err := CheckLocal(); err != nil {
		t.Fatal(err)
	}
}

// CheckLocal reports the first non-local test target. Empty envs are allowed
// so packages that skip without infrastructure still run.
func CheckLocal() error {
	for _, key := range []string{"TEST_DATABASE_URL", "TEST_DATABASE_ADMIN_URL"} {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			continue
		}
		host, err := dsnHost(v)
		if err != nil {
			return fmt.Errorf("%s: cannot parse target: %w", key, err)
		}
		if !isLoopback(host) {
			return fmt.Errorf("%s points at %q, not loopback; DB tests delete data and require a local throwaway database", key, host)
		}
	}
	if v := strings.TrimSpace(os.Getenv("TEST_REDIS_URL")); v != "" {
		host, err := redisHost(v)
		if err != nil {
			return fmt.Errorf("TEST_REDIS_URL: cannot parse target: %w", err)
		}
		if !isLoopback(host) {
			return fmt.Errorf("TEST_REDIS_URL points at %q, not loopback; DB tests call FlushDB and require a local throwaway Redis", host)
		}
	}
	return nil
}

func dsnHost(dsn string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(dsn))
	if err != nil {
		return "", err
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("missing host")
	}
	return u.Hostname(), nil
}

func redisHost(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if !strings.Contains(v, "://") {
		v = "redis://" + v
	}
	u, err := url.Parse(v)
	if err != nil {
		return "", err
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("missing host")
	}
	return u.Hostname(), nil
}

func isLoopback(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
