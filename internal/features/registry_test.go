package features

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestRegisterAndLookup(t *testing.T) {
	ResetForTest()
	Register(Feature{Name: "alpha", Kind: "alpha", DefaultOn: true})
	Register(Feature{Name: "beta", Kind: "beta"})
	if got := All(); len(got) != 2 {
		t.Errorf("All len=%d want 2", len(got))
	}
	if _, ok := Get("alpha"); !ok {
		t.Errorf("Get(alpha) missing")
	}
	if _, ok := Get("ALPHA"); !ok {
		t.Errorf("Get should be case-insensitive")
	}
}

func TestRegister_DuplicateNamePanics(t *testing.T) {
	ResetForTest()
	Register(Feature{Name: "dup", Kind: "dup"})
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on duplicate name")
		}
	}()
	Register(Feature{Name: "dup", Kind: "dup2"})
}

func TestRegister_EmptyNamePanics(t *testing.T) {
	ResetForTest()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on empty name")
		}
	}()
	Register(Feature{Name: ""})
}

func TestAllowedForVertical(t *testing.T) {
	ResetForTest()
	Register(Feature{Name: "a", Kind: "a", VerticalSlugs: nil})
	Register(Feature{Name: "b", Kind: "b", VerticalSlugs: []string{"fnb", "salon"}})
	if !AllowedForVertical("a", "anything") {
		t.Errorf("a should be allowed for any vertical")
	}
	if !AllowedForVertical("b", "fnb") {
		t.Errorf("b should be allowed for fnb")
	}
	if AllowedForVertical("b", "klinik") {
		t.Errorf("b should NOT be allowed for klinik")
	}
	if AllowedForVertical("nonexistent", "fnb") {
		t.Errorf("unknown feature should not be allowed")
	}
}

func TestDefaultSetFor(t *testing.T) {
	d := DefaultSetFor("fnb")
	if len(d) == 0 {
		t.Errorf("fnb defaults empty")
	}
	if !contains(d, "records") {
		t.Errorf("fnb defaults should include records")
	}
	d2 := DefaultSetFor("unknown-vertical")
	if len(d2) == 0 {
		t.Errorf("unknown vertical should still get a default set")
	}
}

func TestIsKnownFeature(t *testing.T) {
	if !IsKnownFeature("records") {
		t.Errorf("records is always known")
	}
	Register(Feature{Name: "knownthing", Kind: "knownthing"})
	if !IsKnownFeature("knownthing") {
		t.Errorf("knownthing should be known after Register")
	}
	if IsKnownFeature("nosuch") {
		t.Errorf("nosuch should not be known")
	}
}

// TestBuildVerticalRouter_UnknownVerticalPanics: a misconfigured
// builder call must die at boot, not in production.
func TestBuildVerticalRouter_UnknownVerticalPanics(t *testing.T) {
	ResetForTest()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic for unknown vertical")
		}
	}()
	BuildVerticalRouter(BuilderDeps{Pool: nil, Cache: nil}, "nosuch")
}

// TestBuildVerticalRouter_DuplicateRoutePattern: the per-vertical
// builder mounts each feature under /<feature>/. Two features that
// pick the same name would collide; the names live in the registry
// so a name collision is already a panic. This test ensures a
// build with two valid features (different names) succeeds.
func TestBuildVerticalRouter_HappyPath(t *testing.T) {
	ResetForTest()
	Register(Feature{Name: "feat1", Kind: "f1", DefaultOn: true, Routes: func(r chi.Router, _ BuilderDeps) {
		r.Get("/ping", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	}})
	Register(Feature{Name: "feat2", Kind: "f2", DefaultOn: true, Routes: func(r chi.Router, _ BuilderDeps) {
		r.Get("/pong", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	}})
	// Build a router; it should not panic.
	r := BuildVerticalRouter(BuilderDeps{Pool: nil, Cache: nil}, "fnb")
	if r == nil {
		t.Errorf("BuildVerticalRouter returned nil")
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
