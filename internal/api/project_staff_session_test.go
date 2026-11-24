package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/davinakmalyasha/agents-proxy/internal/auth"
	"github.com/davinakmalyasha/agents-proxy/internal/authapi"
)

// TestCentrumStaffSession_FullRouter is the end-to-end proof for the staff
// identity path after the entitlement change. It deliberately goes through
// api.NewRouter instead of a feature package's handler: the chain under test
// is /v1/p/<slug> -> project identity -> project/schema gate -> feature gate
// -> tenant transaction -> RLS -> table.
func TestCentrumStaffSession_FullRouter(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	ctx := context.Background()

	cleanup := func() {
		_, _ = h.pool.Exec(ctx, `DELETE FROM project_centrum.tenant_records WHERE tenant_id='centrum'`)
		_, _ = h.pool.Exec(ctx, `DELETE FROM project_centrum.sports_users WHERE tenant_id='centrum' AND email='staff-e2e@test.dev'`)
		_, _ = h.pool.Exec(ctx, `DELETE FROM clients WHERE tenant_id='centrum'`)
	}
	cleanup()
	t.Cleanup(cleanup)

	if _, err := h.pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
		 VALUES ('Centrum Staff E2E','centrum',$1,500000,'active','sports',
		         ARRAY['records','expenses'],'project_centrum')`,
		auth.HashToken("tok-centrum-staff-e2e")); err != nil {
		t.Fatal(err)
	}

	var userID string
	if err := h.pool.QueryRow(ctx,
		`INSERT INTO project_centrum.sports_users (tenant_id, email, password_hash, role, full_name, is_active)
		 VALUES ('centrum','staff-e2e@test.dev','x','owner','Staff E2E',true)
		 RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	raw, hash, err := authapi.GenerateSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO public.staff_sessions (tenant_id, user_id, token_hash, expires_at)
		 VALUES ('centrum', $1, $2, now() + interval '1 hour')`,
		userID, hash); err != nil {
		t.Fatal(err)
	}

	do := func(method, path, header, value, body string) (*http.Response, map[string]any) {
		t.Helper()
		var rdr *bytes.Reader
		if body == "" {
			rdr = bytes.NewReader(nil)
		} else {
			rdr = bytes.NewReader([]byte(body))
		}
		req, err := http.NewRequest(method, srv.URL+path, rdr)
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if header != "" {
			req.Header.Set(header, value)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if resp.Header.Get("Content-Type") != "" {
			_ = json.NewDecoder(resp.Body).Decode(&decoded)
		}
		resp.Body.Close()
		return resp, decoded
	}

	resp, body := do(http.MethodGet, "/v1/p/centrum/records", "X-Session-Token", raw, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("staff records list = %d body=%v, want 200", resp.StatusCode, body)
	}

	resp, body = do(http.MethodPost, "/v1/p/centrum/records", "X-Session-Token", raw,
		`{"kind":"item","title":"Staff E2E","payload":{}}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("staff records create = %d body=%v, want 201", resp.StatusCode, body)
	}

	resp, body = do(http.MethodGet, "/v1/p/centrum/expenses", "X-Session-Token", raw, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("staff expenses list = %d body=%v, want 200", resp.StatusCode, body)
	}

	// Remove the feature from the live tenant row: the next request must be
	// denied even though the staff session itself is still valid.
	if _, err := h.pool.Exec(ctx,
		`UPDATE clients SET features = ARRAY['records'] WHERE tenant_id='centrum'`); err != nil {
		t.Fatal(err)
	}
	resp, body = do(http.MethodGet, "/v1/p/centrum/expenses", "X-Session-Token", raw, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("staff expenses without entitlement = %d body=%v, want 403", resp.StatusCode, body)
	}

	// A valid client token for another tenant must not cross the project
	// boundary, even when the project path names centrum.
	resp, body = do(http.MethodGet, "/v1/p/centrum/records", "X-Client-Token", "tok-alpha", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign token on project route = %d body=%v, want 403", resp.StatusCode, body)
	}
}
