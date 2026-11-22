package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
)

func projectForTest() *agentconfig.Project {
	return &agentconfig.Project{
		Slug:     "acme",
		Vertical: "sports",
		Schema:   "project_acme",
		Features: []string{"records"},
	}
}

func TestDryRunProjectUsesProjectJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--dry-run", "--slug", "centrum", "--name", "Centrum",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"project_centrum", "sports", "facilities"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestDryRunPlatformUsesPublic(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--dry-run", "--platform", "--slug", "mail", "--name", "Mail",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "public") || !strings.Contains(stdout.String(), "fnb") {
		t.Fatalf("stdout=%s want public/fnb", stdout.String())
	}
}

func TestPlatformRejectsProjectFolder(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--dry-run", "--platform", "--slug", "centrum", "--name", "Centrum",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d want 1", code)
	}
	if !strings.Contains(stderr.String(), "has a project folder") {
		t.Errorf("stderr=%s", stderr.String())
	}
}

func TestPlatformRejectsOwnerSeed(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--dry-run", "--platform", "--slug", "mail", "--name", "Mail",
		"--owner-email", "owner@mail.example", "--generate-owner-password",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit=%d want 2", code)
	}
	if !strings.Contains(stderr.String(), "no project staff provider") {
		t.Errorf("stderr=%s", stderr.String())
	}
}

func TestResolveTenantValidatesOverrides(t *testing.T) {
	p := projectForTest()
	if _, _, _, err := resolveTenant(options{vertical: "fnb"}, p); err == nil {
		t.Fatal("vertical mismatch accepted")
	}
	if _, _, _, err := resolveTenant(options{schema: "public"}, p); err == nil {
		t.Fatal("schema mismatch accepted")
	}
	if _, _, _, err := resolveTenant(options{}, nil); err != nil {
		t.Fatalf("platform default: %v", err)
	}
	if _, _, _, err := resolveTenant(options{schema: "project_x"}, nil); err == nil {
		t.Fatal("platform project schema accepted")
	}
}

func TestOwnerPasswordInputs(t *testing.T) {
	pw, err := ownerPassword(options{ownerStdin: true}, strings.NewReader("sekret123\n"))
	if err != nil || pw != "sekret123" {
		t.Fatalf("stdin password=%q err=%v", pw, err)
	}
	if _, err := ownerPassword(options{ownerStdin: true}, strings.NewReader("short")); err == nil {
		t.Fatal("short password accepted")
	}
	gen, err := ownerPassword(options{ownerGen: true}, strings.NewReader(""))
	if err != nil || len(gen) != 36 {
		t.Fatalf("generated=%q len=%d err=%v", gen, len(gen), err)
	}
}
