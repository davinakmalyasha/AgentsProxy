package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/davinakmalyasha/agents-proxy/internal/middleware"
)

// Admin origin registry (migration 058): CRUD contracts, validation
// rejections, and the LoadOrigins DB success path (unit tests cover
// normalization, suffix attacks, and stale-on-error without a DB).
func TestAdmin_OriginsCRUD(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	ctx := context.Background()

	adminReq := func(method, path, token, body string) *http.Response {
		t.Helper()
		var rdr *bytes.Reader
		if body != "" {
			rdr = bytes.NewReader([]byte(body))
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, srv.URL+path, rdr)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		return mustReq(t, req)
	}

	// Wrong auth is rejected before any validation.
	resp := adminReq("POST", "/v1/admin/origins", "wrong", `{"tenant_id":"alpha","origin":"https://x.test"}`)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("wrong auth status=%d want 401", resp.StatusCode)
	}

	// Invalid origins fail loudly, never widen the allowlist.
	for _, bad := range []string{
		`{"tenant_id":"alpha","origin":"*"}`,
		`{"tenant_id":"alpha","origin":"http://example.com"}`,
		`{"tenant_id":"alpha","origin":"https://app.example.id/page"}`,
		`{"tenant_id":"","origin":"https://app.example.id"}`,
		`{"tenant_id":"alpha","origin":"not a url"}`,
	} {
		resp := adminReq("POST", "/v1/admin/origins", h.adminKey, bad)
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("body=%s status=%d want 400", bad, resp.StatusCode)
		}
	}

	// Valid create (trailing slash + case normalized on write).
	resp = adminReq("POST", "/v1/admin/origins", h.adminKey,
		`{"tenant_id":"alpha","origin":"https://App.Alpha.ID/","label":"probe"}`)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "https://app.alpha.id") {
		t.Errorf("create body=%s want normalized origin", raw)
	}
	// Duplicate is idempotent, not a conflict.
	resp = adminReq("POST", "/v1/admin/origins", h.adminKey,
		`{"tenant_id":"alpha","origin":"https://app.alpha.id"}`)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Errorf("duplicate create status=%d want 201", resp.StatusCode)
	}

	// The DB success path: LoadOrigins sees the registered row.
	set, err := middleware.LoadOrigins(ctx, h.pool)
	if err != nil {
		t.Fatalf("LoadOrigins: %v", err)
	}
	if !set.Allowed("https://app.alpha.id") {
		t.Error("LoadOrigins set missing the registered origin")
	}

	// List contains it.
	resp = adminReq("GET", "/v1/admin/origins?tenant_id=alpha", h.adminKey, "")
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(raw), "https://app.alpha.id") {
		t.Errorf("list status=%d body=%s", resp.StatusCode, raw)
	}

	// Delete, then 404 on repeat; LoadOrigins forgets it.
	resp = adminReq("DELETE", "/v1/admin/origins?tenant_id=alpha&origin=https://app.alpha.id", h.adminKey, "")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("delete status=%d want 200", resp.StatusCode)
	}
	resp = adminReq("DELETE", "/v1/admin/origins?tenant_id=alpha&origin=https://app.alpha.id", h.adminKey, "")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("re-delete status=%d want 404", resp.StatusCode)
	}
	set, err = middleware.LoadOrigins(ctx, h.pool)
	if err != nil {
		t.Fatalf("LoadOrigins after delete: %v", err)
	}
	if set.Allowed("https://app.alpha.id") {
		t.Error("deleted origin still allowed")
	}

	// Suffix rules: bare/Star forms rejected, dot + hyphen forms round-trip.
	for _, bad := range []string{`{"suffix":"example.com"}`, `{"suffix":"*"}`, `{"suffix":"https://x.com"}`, `{"suffix":"-vercel.app"}`} {
		resp := adminReq("POST", "/v1/admin/origin-suffixes", h.adminKey, bad)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("suffix body=%s status=%d want 400", bad, resp.StatusCode)
		}
	}
	resp = adminReq("POST", "/v1/admin/origin-suffixes", h.adminKey, `{"suffix":".alpha.id","note":"probe"}`)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Errorf("suffix create status=%d want 201", resp.StatusCode)
	}
	set, err = middleware.LoadOrigins(ctx, h.pool)
	if err != nil {
		t.Fatalf("LoadOrigins suffix: %v", err)
	}
	if !set.Allowed("https://anything.alpha.id") || set.Allowed("https://evil-alpha.id") {
		t.Error("suffix rule misapplied through the DB path")
	}
	resp = adminReq("DELETE", "/v1/admin/origin-suffixes?suffix=.alpha.id", h.adminKey, "")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("suffix delete status=%d want 200", resp.StatusCode)
	}

	// Hyphen-boundary form round-trips too (provider team namespaces).
	resp = adminReq("POST", "/v1/admin/origin-suffixes", h.adminKey, `{"suffix":"-team.preview.example","note":"probe-hyphen"}`)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Errorf("hyphen suffix create status=%d want 201", resp.StatusCode)
	}
	set, err = middleware.LoadOrigins(ctx, h.pool)
	if err != nil {
		t.Fatalf("LoadOrigins hyphen suffix: %v", err)
	}
	if !set.Allowed("https://centrum-badminton-9i427fi8w-team.preview.example") ||
		set.Allowed("https://team.preview.example") {
		t.Error("hyphen suffix rule misapplied through the DB path")
	}
	resp = adminReq("DELETE", "/v1/admin/origin-suffixes?suffix=-team.preview.example", h.adminKey, "")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("hyphen suffix delete status=%d want 200", resp.StatusCode)
	}
}

// Provisioning hook: origins[] validated before the client insert
// (bad origin = 400, no half-created tenant) and stored after it.
func TestAdmin_Create_WithOrigins(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()

	post := func(body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+"/v1/admin/clients", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+h.adminKey)
		resp := mustReq(t, req)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	if code, _ := post(`{"name":"op1","tenant_id":"orig-probe-bad","origins":["*"]}`); code != 400 {
		t.Errorf("bad-origin provisioning status=%d want 400", code)
	}
	// No half-created tenant.
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM clients WHERE tenant_id='orig-probe-bad'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("bad-origin provisioning created the client anyway")
	}

	if code, _ := post(`{"name":"op2","tenant_id":"orig-probe","origins":["https://op2.example.id"]}`); code != 201 {
		t.Errorf("provisioning status=%d want 201", code)
	}
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM frontend_origins WHERE tenant_id='orig-probe' AND origin='https://op2.example.id'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("provisioned origin row missing")
	}
}
