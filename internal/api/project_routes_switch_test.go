package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/davinakmalyasha/agents-proxy/internal/auth"
	"github.com/davinakmalyasha/agents-proxy/internal/features"
)

// TestLegacyProjectRoutesOff pins the retirement switch: a project-owned
// vertical is not mounted under /v1/{vertical} when the flag is off, while the
// project-scoped router keeps working.
func TestLegacyProjectRoutesOff(t *testing.T) {
	features.SetLegacyProjectRoutes(false)
	defer features.SetLegacyProjectRoutes(true)
	h := setup(t)
	srv := httptest.NewServer(h.router)
	defer srv.Close()
	ctx := context.Background()

	cleanup := func() {
		_, _ = h.pool.Exec(ctx, `DELETE FROM project_centrum.sports_facilities WHERE tenant_id='centrum' AND name='Switch Court'`)
		_, _ = h.pool.Exec(ctx, `DELETE FROM clients WHERE tenant_id='centrum'`)
	}
	cleanup()
	t.Cleanup(cleanup)

	if _, err := h.pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
		 VALUES ('Centrum Switch','centrum',$1,500000,'active','sports',
		         ARRAY['records','facilities','pricing','availability'],'project_centrum')`,
		auth.HashToken("tok-centrum-switch")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO project_centrum.sports_facilities (tenant_id,name,type,price_per_hour,open_time,close_time)
		 VALUES ('centrum','Switch Court','badminton',50000,'07:00','22:00')`); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(srv.URL + "/v1/sports/facilities?tenant_id=centrum")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("legacy alias status=%d want 404", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/v1/p/centrum/facilities")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("project route status=%d want 200", resp.StatusCode)
	}
}
