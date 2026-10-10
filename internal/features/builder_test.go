package features

import (
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestVerticalsToMount_EmptyFallsBackToAll(t *testing.T) {
	got := VerticalsToMount(nil)
	if len(got) != len(AllowedVerticals) {
		t.Fatalf("nil live mounted %d verticals, want all %d", len(got), len(AllowedVerticals))
	}
	got = VerticalsToMount([]string{})
	if len(got) != len(AllowedVerticals) {
		t.Fatalf("empty live mounted %d verticals, want all %d", len(got), len(AllowedVerticals))
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
