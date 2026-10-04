package features

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/features/clientctx"
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
