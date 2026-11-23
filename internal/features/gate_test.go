package features

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/features/clientctx"
	"github.com/davinakmalyasha/agents-proxy/internal/tenanthealth"
)

func TestRequireFeature_OffReturns403(t *testing.T) {
	ResetForTest()
	Register(Feature{Name: "x", Kind: "x"})
	h := RequireFeature("x")(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }),
	)
	ctx := clientctx.WithClient(context.Background(), &cache.ClientModel{
		Vertical: "fnb",
		Features: []string{"records"}, // x not in here
	})
	req := httptest.NewRequest("GET", "/x", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req.WithContext(ctx))
	if rr.Code != 403 {
		t.Errorf("code=%d want 403", rr.Code)
	}
}

func TestRequireFeature_OnPasses(t *testing.T) {
	ResetForTest()
	Register(Feature{Name: "x", Kind: "x"})
	called := false
	h := RequireFeature("x")(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(200)
		}),
	)
	ctx := clientctx.WithClient(context.Background(), &cache.ClientModel{
		Vertical: "fnb",
		Features: []string{"x"},
	})
	req := httptest.NewRequest("GET", "/x", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req.WithContext(ctx))
	if rr.Code != 200 {
		t.Errorf("code=%d want 200", rr.Code)
	}
	if !called {
		t.Errorf("handler not called")
	}
}

func TestRequireFeature_VerticalMismatch403(t *testing.T) {
	ResetForTest()
	Register(Feature{Name: "klinikonly", Kind: "k", VerticalSlugs: []string{"klinik"}})
	h := RequireFeature("klinikonly")(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }),
	)
	ctx := clientctx.WithClient(context.Background(), &cache.ClientModel{
		Vertical: "fnb", // not in VerticalSlugs
		Features: []string{"klinikonly"},
	})
	req := httptest.NewRequest("GET", "/x", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req.WithContext(ctx))
	if rr.Code != 403 {
		t.Errorf("code=%d want 403", rr.Code)
	}
}

func TestRequireFeature_NoClientReturns403(t *testing.T) {
	ResetForTest()
	Register(Feature{Name: "x", Kind: "x"})
	h := RequireFeature("x")(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }),
	)
	req := httptest.NewRequest("GET", "/x", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req) // no clientctx.With
	if rr.Code != 403 {
		t.Errorf("code=%d want 403", rr.Code)
	}
}

func TestRequireTenantHealthy_Quarantined503(t *testing.T) {
	tenanthealth.Quarantine("sick", "test")
	defer tenanthealth.Release("sick")
	called := false
	h := RequireTenantHealthy(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(200) }),
	)
	ctx := clientctx.WithClient(context.Background(), &cache.ClientModel{
		Vertical: "sports", TenantID: "sick", Features: []string{"records"},
	})
	req := httptest.NewRequest("GET", "/x", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req.WithContext(ctx))
	if rr.Code != 503 {
		t.Errorf("code=%d want 503", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, tenanthealth.ResponseBody) {
		t.Errorf("body=%q want distinct quarantine body", body)
	}
	if called {
		t.Error("handler ran for quarantined tenant")
	}
}

func TestRequireTenantHealthy_HealthyPasses(t *testing.T) {
	called := false
	h := RequireTenantHealthy(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(200) }),
	)
	ctx := clientctx.WithClient(context.Background(), &cache.ClientModel{
		Vertical: "sports", TenantID: "fine", Features: []string{"records"},
	})
	req := httptest.NewRequest("GET", "/x", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req.WithContext(ctx))
	if rr.Code != 200 || !called {
		t.Errorf("code=%d called=%v want 200/true", rr.Code, called)
	}
}

func TestRequireTenantHealthy_NoIdentityPasses(t *testing.T) {
	// No identity (auth answers those): the gate only fences explicit slugs.
	called := false
	h := RequireTenantHealthy(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(200) }),
	)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/x", nil))
	if rr.Code != 200 || !called {
		t.Errorf("code=%d called=%v want 200/true", rr.Code, called)
	}
}

func TestRequireFeature_PublicRoleIsNotStaffBypass(t *testing.T) {
	ResetForTest()
	Register(Feature{Name: "x", Kind: "x"})
	h := RequireFeature("x")(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }),
	)
	ctx := clientctx.With(context.Background(), &clientctx.Identity{
		Role: clientctx.RolePublic, Tenant: "acme",
	})
	req := httptest.NewRequest("GET", "/x", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req.WithContext(ctx))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("public identity status=%d, want 403", rr.Code)
	}
}

func TestRequireFeature_StaffStillNeedsEntitlement(t *testing.T) {
	ResetForTest()
	Register(Feature{Name: "x", Kind: "x"})
	h := RequireFeature("x")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))

	ctx := clientctx.With(context.Background(), &clientctx.Identity{
		Role: clientctx.RoleOwner, Tenant: "acme", Vertical: "fnb", Features: []string{"records"},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil).WithContext(ctx))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("staff without feature = %d, want 403", rec.Code)
	}

	ctx = clientctx.With(context.Background(), &clientctx.Identity{
		Role: clientctx.RoleOwner, Tenant: "acme", Vertical: "fnb", Features: []string{"x"},
	})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil).WithContext(ctx))
	if rec.Code != http.StatusOK {
		t.Fatalf("staff with feature = %d, want 200", rec.Code)
	}
}

func TestRequireProjectFeature_PublicReadStillNeedsEntitlement(t *testing.T) {
	h := RequireProjectFeature("acme", "x", true)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }),
	)
	public := &clientctx.Identity{Role: clientctx.RolePublic, Tenant: "acme", Vertical: "fnb"}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil).WithContext(
		clientctx.With(context.Background(), public)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("public without entitlement = %d, want 403", rec.Code)
	}

	public.Features = []string{"x"}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil).WithContext(
		clientctx.With(context.Background(), public)))
	if rec.Code != http.StatusOK {
		t.Fatalf("public with entitlement = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/x", nil).WithContext(
		clientctx.With(context.Background(), public)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("public write = %d, want 403", rec.Code)
	}

	staff := &clientctx.Identity{
		Role: clientctx.RoleStaff, Tenant: "acme", Vertical: "fnb", Features: []string{"records"},
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil).WithContext(
		clientctx.With(context.Background(), staff)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("staff without entitlement = %d, want 403", rec.Code)
	}
}
