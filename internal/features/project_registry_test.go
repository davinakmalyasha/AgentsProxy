package features

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/davinakmalyasha/agents-proxy/internal/features/clientctx"
)

func TestRegisterProjectIsScoped(t *testing.T) {
	resetProjectRegistryForTest()
	RegisterProject("acme", "sports", Feature{Name: "bookings"})
	RegisterProject("beta", "sports", Feature{Name: "bookings"})

	if got := ProjectFeatures("acme"); len(got) != 1 || got[0].Name != "bookings" {
		t.Fatalf("acme features = %#v", got)
	}
	if got := ProjectFeatures("beta"); len(got) != 1 || got[0].Name != "bookings" {
		t.Fatalf("beta features = %#v", got)
	}
	if got := RegisteredProjects(); len(got) != 2 || got[0] != "acme" || got[1] != "beta" {
		t.Fatalf("registered projects = %v", got)
	}
	if got := ProjectVertical("acme"); got != "sports" {
		t.Fatalf("vertical = %q", got)
	}
}

func TestProjectRegistrationStaysScoped(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	RegisterProject("centrum", "sports", Feature{Name: "quote"})
	if _, ok := Get("quote"); ok {
		t.Fatal("project feature leaked into the global vertical registry")
	}
	if !IsKnownProjectFeature("centrum", "quote") {
		t.Fatal("project registration missing from the project registry")
	}
	if IsKnownProjectFeature("other", "quote") {
		t.Fatal("project feature leaked into another project")
	}
}

func TestRegisterProjectDuplicatePanics(t *testing.T) {
	resetProjectRegistryForTest()
	RegisterProject("acme", "sports", Feature{Name: "bookings"})
	defer func() {
		if recover() == nil {
			t.Fatal("expected a differing duplicate project feature to panic")
		}
	}()
	RegisterProject("acme", "sports", Feature{Name: "bookings", Kind: "different"})
}

func TestRegisterProjectIdenticalIsIdempotent(t *testing.T) {
	resetProjectRegistryForTest()
	defer resetProjectRegistryForTest()
	RegisterProject("acme", "sports", Feature{Name: "bookings"})
	RegisterProject("acme", "sports", Feature{Name: "bookings"})
	if got := ProjectFeatures("acme"); len(got) != 1 {
		t.Fatalf("features=%v, want one", got)
	}
}

func TestRequireProjectRejectsMismatch(t *testing.T) {
	h := RequireProject("acme", "project_acme")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(clientctx.With(req.Context(), &clientctx.Identity{
		Role: clientctx.RoleClient, Tenant: "beta", DBSchema: "project_beta",
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestRequireProjectRejectsSchemaMismatch(t *testing.T) {
	h := RequireProject("acme", "project_acme")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(clientctx.With(req.Context(), &clientctx.Identity{
		Role: clientctx.RoleClient, Tenant: "acme", DBSchema: "public",
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a project URL backed by public", rr.Code)
	}
}

func TestRequireProjectAcceptsMatchingTenantAndSchema(t *testing.T) {
	h := RequireProject("acme", "project_acme")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(clientctx.With(req.Context(), &clientctx.Identity{
		Role: clientctx.RoleClient, Tenant: "acme", DBSchema: "project_acme",
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestRequirePlatformTenantRejectsProjectTenant(t *testing.T) {
	h := RequirePlatformTenant(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(clientctx.With(req.Context(), &clientctx.Identity{
		Role: clientctx.RoleClient, Tenant: "acme", DBSchema: "project_acme",
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestRequirePlatformTenantAcceptsPublicSchema(t *testing.T) {
	h := RequirePlatformTenant(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(clientctx.With(req.Context(), &clientctx.Identity{
		Role: clientctx.RoleClient, Tenant: "acme", DBSchema: "public",
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestRegisterProjectRejectsUnknownVertical(t *testing.T) {
	resetProjectRegistryForTest()
	defer resetProjectRegistryForTest()
	defer func() {
		if recover() == nil {
			t.Fatal("expected unknown vertical to panic")
		}
	}()
	RegisterProject("acme", "not-a-vertical", Feature{Name: "quote"})
}

func TestRegisterProjectRejectsVerticalDrift(t *testing.T) {
	resetProjectRegistryForTest()
	defer resetProjectRegistryForTest()
	RegisterProject("acme", "sports", Feature{Name: "quote"})
	defer func() {
		if recover() == nil {
			t.Fatal("expected a second vertical for one project to panic")
		}
	}()
	RegisterProject("acme", "fnb", Feature{Name: "menu"})
}

func TestRegisterProjectRejectsUnsafeFeatureName(t *testing.T) {
	resetProjectRegistryForTest()
	defer resetProjectRegistryForTest()
	defer func() {
		if recover() == nil {
			t.Fatal("expected a path-like feature name to panic")
		}
	}()
	RegisterProject("acme", "sports", Feature{Name: "bad/name"})
}
