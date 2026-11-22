package features

import (
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestVerticalsToMount_NilFallsBackToAll(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	got := VerticalsToMount(nil)
	if len(got) != len(AllowedVerticals) {
		t.Fatalf("nil live mounted %d verticals, want all %d", len(got), len(AllowedVerticals))
	}
	// A non-nil empty slice means the query succeeded and found nothing.
	// It must not mount every vertical, or a platform-only deployment
	// would resurrect /v1/sports on an empty database.
	if got := VerticalsToMount([]string{}); len(got) != 0 {
		t.Fatalf("empty live mounted %v, want none", got)
	}
}

func TestVerticalsToMount_PlatformOnly(t *testing.T) {
	ResetForTest()
	SetLegacyProjectRoutes(false)
	defer func() {
		SetLegacyProjectRoutes(true)
		ResetForTest()
	}()
	if got := VerticalsToMount(nil); len(got) != 0 {
		t.Fatalf("platform-only lookup failure mounted %v, want none", got)
	}
	if got := VerticalsToMount([]string{}); len(got) != 0 {
		t.Fatalf("platform-only empty mounted %v, want none", got)
	}
	got := VerticalsToMount([]string{"fnb"})
	if len(got) != 1 || got[0] != "fnb" {
		t.Fatalf("platform-only live mounted %v, want [fnb]", got)
	}
}

func TestVerticalsToMount_LiveOnly(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	// No custom features registered: only the live vertical mounts.
	got := VerticalsToMount([]string{"sports"})
	if len(got) != 1 || got[0] != "sports" {
		t.Fatalf("live=[sports] mounted %v", got)
	}
}

func TestVerticalsToMount_UnknownVerticalIgnored(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	got := VerticalsToMount([]string{"sports", "nope"})
	if len(got) != 1 || got[0] != "sports" {
		t.Fatalf("unknown vertical should be ignored, mounted %v", got)
	}
}

func TestVerticalsToMount_AdapterOnlyDoesNotForceMount(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	Register(Feature{Name: "customx", VerticalSlugs: []string{"fnb"}})
	if got := VerticalsToMount([]string{"sports"}); len(got) != 1 {
		t.Fatalf("adapter-only feature must not force a mount, got %v", got)
	}
}

func TestVerticalsToMount_CustomRoutesAlwaysMount(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	Register(Feature{
		Name:          "customy",
		VerticalSlugs: []string{"fnb"},
		Routes:        func(chi.Router, BuilderDeps) {},
	})
	got := VerticalsToMount([]string{"sports"})
	if len(got) != 2 {
		t.Fatalf("custom-route vertical must mount, got %v", got)
	}
}
