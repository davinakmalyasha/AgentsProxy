package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/davinakmalyasha/agents-proxy/internal/auth"
	"github.com/davinakmalyasha/agents-proxy/internal/features"
)

// TestAdminPatchProjectVerticalIsImmutable pins the manifest boundary: a
// project tenant's vertical is owned by project.json. Admin PATCH may change
// quota/status/features, but moving the vertical would leave the router,
// project feature set, and tables pointing at a different domain.
func TestAdminPatchProjectVerticalIsImmutable(t *testing.T) {
	h := setup(t)
	// Alpha has a project schema in the API fixture; give the admin catalog
	// the matching manifest so Patch sees it as a project tenant.
	h.projects.add("alpha", "project_alpha", "fnb", []string{"records"}, 1, true)

	var id string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT id::text FROM clients WHERE tenant_id = 'alpha'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.router)
	defer srv.Close()

	patch := func(body string) int {
		req, err := http.NewRequest(http.MethodPatch,
			srv.URL+"/v1/admin/clients/"+id, bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+h.adminKey)
		req.Header.Set("Content-Type", "application/json")
		resp := mustReq(t, req)
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := patch(`{"vertical":"sports"}`); code != http.StatusBadRequest {
		t.Fatalf("project vertical change = %d, want 400", code)
	}
	if code := patch(`{"vertical":"fnb"}`); code != http.StatusOK {
		t.Fatalf("same project vertical = %d, want 200", code)
	}
}

// TestAdminPatchPlatformVerticalRevalidatesFeatures pins the other half of
// the vertical contract: a platform tenant may move, but only to a vertical
// where its existing features are still valid. The effective set is checked,
// so dropping an incompatible feature in the same PATCH is allowed.
func TestAdminPatchPlatformVerticalRevalidatesFeatures(t *testing.T) {
	const featureName = "platform-revalidate-test"
	if !features.IsKnownFeature(featureName) {
		features.Register(features.Feature{
			Name:          featureName,
			Kind:          "platform_revalidate_test",
			VerticalSlugs: []string{"sports"},
		})
	}

	h := setup(t)
	ctx := context.Background()
	_, err := h.pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
		 VALUES ('revalidate', 'revalidate', $1, 5000, 'active', 'sports', $2, 'public')`,
		auth.HashToken("tok-revalidate"), []string{"records", featureName})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = h.pool.Exec(context.Background(), `DELETE FROM clients WHERE tenant_id = 'revalidate'`)
	})

	var id string
	if err := h.pool.QueryRow(ctx,
		`SELECT id::text FROM clients WHERE tenant_id = 'revalidate'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.router)
	defer srv.Close()

	patch := func(body string) int {
		req, err := http.NewRequest(http.MethodPatch,
			srv.URL+"/v1/admin/clients/"+id, bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+h.adminKey)
		req.Header.Set("Content-Type", "application/json")
		resp := mustReq(t, req)
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// Moving to a vertical that rejects an existing feature is refused,
	// even though the feature itself is not in the request.
	if code := patch(`{"vertical":"klinik"}`); code != http.StatusBadRequest {
		t.Fatalf("vertical with incompatible feature = %d, want 400", code)
	}
	// Dropping the incompatible feature in the same request makes the
	// effective set valid, so the move succeeds.
	if code := patch(`{"vertical":"klinik","features":["records"]}`); code != http.StatusOK {
		t.Fatalf("vertical with reconciled features = %d, want 200", code)
	}
	// Input is canonicalized before it is stored: the gate matches feature
	// names exactly, so " Records " must not become a dead entitlement.
	if code := patch(`{"features":[" Records ","records"]}`); code != http.StatusOK {
		t.Fatalf("canonical feature patch = %d, want 200", code)
	}
	var stored []string
	if err := h.pool.QueryRow(ctx,
		`SELECT features FROM clients WHERE tenant_id = 'revalidate'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0] != "records" {
		t.Fatalf("stored features = %v, want [records]", stored)
	}
}
