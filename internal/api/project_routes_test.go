package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/davinakmalyasha/agents-proxy/internal/auth"
	_ "github.com/davinakmalyasha/agents-proxy/projects/centrum/backend/facilities"
)

// TestCentrumProjectRoutes proves the final cutover surface: Centrum is
// reachable only under /v1/p/centrum/..., the old vertical alias is gone, and
// the project gate still rejects another tenant.
func TestCentrumProjectRoutes(t *testing.T) {
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	ctx := context.Background()

	cleanup := func() {
		_, _ = h.pool.Exec(ctx, `DELETE FROM project_centrum.sports_facilities WHERE tenant_id='centrum' AND name='Parity Court'`)
		_, _ = h.pool.Exec(ctx, `DELETE FROM clients WHERE tenant_id='centrum'`)
	}
	cleanup()
	t.Cleanup(cleanup)

	if _, err := h.pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
		 VALUES ('Centrum Test','centrum',$1,500000,'active','sports',
		         ARRAY['records','availability','bookings','courtblocks','customers','expenses','facilities','inventory','kantin','members','payments','pricing','series','users'],'project_centrum')`,
		auth.HashToken("tok-centrum-test")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO project_centrum.sports_facilities (tenant_id,name,type,price_per_hour,open_time,close_time)
		 VALUES ('centrum','Parity Court','badminton',50000,'07:00','22:00')`); err != nil {
		t.Fatal(err)
	}

	get := func(path string, header, value string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if header != "" {
			req.Header.Set(header, value)
		}
		return mustReq(t, req)
	}

	if resp := get("/v1/sports/facilities?tenant_id=centrum", "", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("legacy alias status=%d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	resp := get("/v1/p/centrum/facilities", "", "")
	body := readJSON(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/p/centrum/facilities status=%d body=%v", resp.StatusCode, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("/v1/p/centrum/facilities items=%v, want one facility", items)
	}

	if resp := get("/v1/p/centrum/bookings", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("private project read status=%d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// The router mounts /records unconditionally; the project schema must
	// actually contain tenant_records (migration 0002).
	if resp := get("/v1/p/centrum/records", "X-Client-Token", "tok-centrum-test"); resp.StatusCode != http.StatusOK {
		body := readJSON(t, resp)
		t.Fatalf("records read status=%d body=%v, want 200", resp.StatusCode, body)
	} else {
		body := readJSON(t, resp)
		if items, _ := body["items"].([]any); len(items) != 0 {
			t.Fatalf("records items=%v, want empty", items)
		}
	}

	if resp := get("/v1/p/centrum/facilities", "X-Client-Token", "tok-alpha"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-project read status=%d, want 403", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}
