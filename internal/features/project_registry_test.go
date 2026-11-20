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

func TestRegisterProjectDuplicatePanics(t *testing.T) {
	resetProjectRegistryForTest()
	RegisterProject("acme", "sports", Feature{Name: "bookings"})
	defer func() {
		if recover() == nil {
			t.Fatal("expected duplicate project feature to panic")
		}
	}()
	RegisterProject("acme", "sports", Feature{Name: "bookings"})
}

func TestRequireProjectRejectsMismatch(t *testing.T) {
	h := RequireProject("acme")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(clientctx.With(req.Context(), &clientctx.Identity{Role: clientctx.RoleClient, Tenant: "beta"}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}
