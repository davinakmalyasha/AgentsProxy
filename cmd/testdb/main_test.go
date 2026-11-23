package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/davinakmalyasha/agents-proxy/internal/testsafety"
)

func TestFreshName(t *testing.T) {
	a := freshName("agents_proxy_test")
	b := freshName("agents_proxy_test")
	if a == b {
		t.Fatalf("freshName returned %q twice", a)
	}
	if !strings.HasPrefix(a, "agents_proxy_test_") || len(a) > 63 || !dbNameRe.MatchString(a) {
		t.Fatalf("invalid name %q", a)
	}
}

func TestParseLocalURL(t *testing.T) {
	u, err := parseLocalURL("postgres://postgres@127.0.0.1:5432/agents_proxy_test?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if got := maintenanceURL(u).Path; got != "/postgres" {
		t.Fatalf("maintenance path = %q", got)
	}
	for _, raw := range []string{
		"postgres://postgres@db.example.com:5432/agents_proxy_test",
		"postgres://postgres@10.0.0.5:5432/agents_proxy_test",
		"mysql://postgres@localhost/agents_proxy_test",
		"postgres://postgres@localhost:5432/",
	} {
		if _, err := parseLocalURL(raw); err == nil {
			t.Errorf("parseLocalURL(%q) succeeded, want refusal", raw)
		}
	}
}

func TestDropRefusesUnmanagedName(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{
		"-action", "drop",
		"-admin-url", "postgres://postgres@127.0.0.1:5432/postgres",
		"-prefix", "agents_proxy_test",
		"-name", "postgres",
	}, &out, &errOut)
	if code == 0 {
		t.Fatalf("drop postgres succeeded: %s", out.String())
	}
	if !strings.Contains(errOut.String(), "refusing to drop") {
		t.Fatalf("error = %q, want refusal", errOut.String())
	}
}

func TestCreateDropIntegration(t *testing.T) {
	testsafety.RequireLocal(t)
	admin := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if admin == "" {
		t.Skip("TEST_DATABASE_ADMIN_URL not set — skipping integration test")
	}
	var out, errOut bytes.Buffer
	code := run([]string{
		"-action", "create",
		"-admin-url", admin,
		"-prefix", "agents_proxy_test",
		"-json",
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("create code=%d stderr=%s", code, errOut.String())
	}
	var created output
	if err := json.Unmarshal(out.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v\n%s", err, out.String())
	}
	if created.Name == "" || !strings.HasPrefix(created.Name, "agents_proxy_test_") {
		t.Fatalf("created name = %q", created.Name)
	}
	t.Cleanup(func() {
		var dOut, dErr bytes.Buffer
		_ = run([]string{
			"-action", "drop",
			"-admin-url", admin,
			"-prefix", "agents_proxy_test",
			"-name", created.Name,
		}, &dOut, &dErr)
	})
	if err := drop(context.Background(), admin, "agents_proxy_test", created.Name); err != nil {
		t.Fatal(err)
	}
}
