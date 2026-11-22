package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestCreatePrintsTokenOnceAndSendsProjectSchema(t *testing.T) {
	var got createRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/admin/clients" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("authorization=%q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"id-1","name":"Acme","tenant_id":"acme","token":"tok_once","monthly_quota_tokens":500000,"vertical":"sports","features":["records"],"schema":"project_acme"}`))
	}))
	defer srv.Close()
	t.Setenv("AGENTS_PROXY_URL", srv.URL)
	t.Setenv("ADMIN_API_KEY", "test-key")

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "projects", "acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "projects", "acme", "project.json"),
		[]byte(`{"slug":"acme","vertical":"sports","schema":"project_acme"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	code, stdout, stderr := runCLI(t, "create", "--slug", "acme", "--name", "Acme")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "tok_once") {
		t.Errorf("token not printed: %s", stdout)
	}
	if !strings.Contains(stderr, "will not show it again") {
		t.Errorf("missing one-time warning: %s", stderr)
	}
	if got.TenantID != "acme" || got.Vertical != "sports" || got.Schema != "project_acme" {
		t.Errorf("request=%+v", got)
	}
	if got.MonthlyQuotaTokens != 0 {
		t.Errorf("quota=%d, want server default 0", got.MonthlyQuotaTokens)
	}
}

func TestCreatePlatformUsesPublicSchema(t *testing.T) {
	var got createRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"id-1","name":"Mail","tenant_id":"mail","token":"tok_once","monthly_quota_tokens":500000,"vertical":"fnb","features":["records"],"schema":"public"}`))
	}))
	defer srv.Close()
	t.Setenv("AGENTS_PROXY_URL", srv.URL)
	t.Setenv("ADMIN_API_KEY", "test-key")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runCLI(t, "create", "--slug", "mail", "--name", "Mail", "--platform")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if got.Schema != "public" || got.TenantID != "mail" {
		t.Fatalf("request=%+v want schema=public tenant=mail", got)
	}
	if !strings.Contains(stdout, "tok_once") {
		t.Errorf("token not printed: %s", stdout)
	}
}

func TestCreatePlatformRejectsExistingProjectFolder(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "projects", "acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "projects", "acme", "project.json"),
		[]byte(`{"slug":"acme","vertical":"sports","schema":"project_acme"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	code, _, stderr := runCLI(t, "create", "--slug", "acme", "--name", "Acme", "--platform")
	if code != 1 {
		t.Fatalf("exit=%d want 1", code)
	}
	if !strings.Contains(stderr, "has a project folder") {
		t.Errorf("stderr=%s", stderr)
	}
}

func TestCreateRequiresHTTPSOutsideLoopback(t *testing.T) {
	t.Setenv("AGENTS_PROXY_URL", "http://example.com")
	t.Setenv("ADMIN_API_KEY", "test-key")
	code, _, stderr := runCLI(t, "create", "--slug", "acme", "--name", "Acme", "--allow-missing-project")
	if code != 1 {
		t.Fatalf("exit=%d, want 1", code)
	}
	if !strings.Contains(stderr, "plain HTTP") {
		t.Errorf("stderr=%s", stderr)
	}
}

func TestListRendersRows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/admin/clients" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("limit") != "5" {
			t.Errorf("limit=%q", r.URL.Query().Get("limit"))
		}
		_, _ = io.WriteString(w, `[{"id":"id-1","name":"Acme","tenant_id":"acme","vertical":"sports","features":["records"],"status":"active","monthly_quota_tokens":500000,"schema":"project_acme"}]`)
	}))
	defer srv.Close()
	t.Setenv("AGENTS_PROXY_URL", srv.URL)
	t.Setenv("ADMIN_API_KEY", "test-key")

	code, stdout, stderr := runCLI(t, "list", "--limit", "5")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{"TENANT", "acme", "project_acme", "records"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestPatchResolvesTenantToID(t *testing.T) {
	var patched patchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/clients":
			_, _ = io.WriteString(w, `[{"id":"id-1","tenant_id":"acme"}]`)
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/admin/clients/id-1":
			if err := json.NewDecoder(r.Body).Decode(&patched); err != nil {
				t.Errorf("decode: %v", err)
			}
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("AGENTS_PROXY_URL", srv.URL)
	t.Setenv("ADMIN_API_KEY", "test-key")

	code, stdout, stderr := runCLI(t, "patch", "--tenant", "acme", "--quota", "123", "--features", "")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if patched.MonthlyQuotaTokens == nil || *patched.MonthlyQuotaTokens != 123 {
		t.Errorf("quota=%v", patched.MonthlyQuotaTokens)
	}
	if patched.Features == nil || len(*patched.Features) != 0 {
		t.Errorf("features=%v, want empty list", patched.Features)
	}
	if !strings.Contains(stdout, "id-1") {
		t.Errorf("stdout=%s", stdout)
	}
}

func TestUsageValidatesTime(t *testing.T) {
	t.Setenv("AGENTS_PROXY_URL", "http://localhost:1")
	t.Setenv("ADMIN_API_KEY", "test-key")
	code, _, stderr := runCLI(t, "usage", "--from", "yesterday")
	if code != 2 {
		t.Fatalf("exit=%d, want 2", code)
	}
	if !strings.Contains(stderr, "RFC3339") {
		t.Errorf("stderr=%s", stderr)
	}
}
