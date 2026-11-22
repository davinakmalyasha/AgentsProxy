package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/davinakmalyasha/agents-proxy/internal/auth"
)

// TestPlatformSurfaceRejectsProjectTenant is the regression for the route
// contract: a project tenant must never be served by /v1/{vertical}, even
// when a platform tenant of the same vertical is live.
func TestPlatformSurfaceRejectsProjectTenant(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	ctx := context.Background()

	// setup seeds project alpha (fnb, project schema) and platform charlie
	// (fnb, public). /v1/fnb is therefore mounted.
	req := func(path, token string) int {
		r, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			r.Header.Set("X-Client-Token", token)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := req("/v1/fnb/records", "tok-alpha"); code != http.StatusNotFound {
		t.Fatalf("project token on /v1/fnb/records = %d, want 404", code)
	}
	if code := req("/v1/fnb/records", "tok-charlie"); code != http.StatusOK {
		t.Fatalf("platform token on /v1/fnb/records = %d, want 200", code)
	}
	if code := req("/v1/p/centrum/records", "tok-charlie"); code != http.StatusForbidden {
		t.Fatalf("platform token on /v1/p/centrum/records = %d, want 403", code)
	}

	// A platform tenant created after the router was built must be reachable:
	// the platform surface is static, not a boot-time snapshot of live rows.
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
		 VALUES ('Delta','delta',$1,500000,'active','klinik',ARRAY['records'],'public')`,
		auth.HashToken("tok-delta")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = h.pool.Exec(context.Background(), `DELETE FROM clients WHERE tenant_id='delta'`)
	})
	if code := req("/v1/klinik/records", "tok-delta"); code != http.StatusOK {
		t.Fatalf("new platform vertical /v1/klinik/records = %d, want 200", code)
	}
}

// TestProjectRouteRejectsWrongSchema pins the URL-to-schema boundary: a
// tenant named centrum whose row points at public must not be served by the
// project router, even though the slug matches.
func TestProjectRouteRejectsWrongSchema(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = h.pool.Exec(context.Background(), `DELETE FROM clients WHERE tenant_id='centrum'`)
	})
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
		 VALUES ('Centrum Wrong Schema','centrum',$1,500000,'active','sports',ARRAY['records'],'public')`,
		auth.HashToken("tok-centrum-public")); err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/p/centrum/records", nil)
	r.Header.Set("X-Client-Token", "tok-centrum-public")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("project URL backed by public schema = %d, want 403", resp.StatusCode)
	}
}
