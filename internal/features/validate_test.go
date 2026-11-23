package features

import (
	"errors"
	"strings"
	"testing"

	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
)

func TestValidateProjectDeclarations(t *testing.T) {
	resetProjectRegistryForTest()
	RegisterProject("acme", "sports", Feature{Name: "facilities"})

	projects := map[string]*agentconfig.Project{
		"acme": {
			Slug:     "acme",
			Vertical: "sports",
			Features: []string{"records", "facilities"},
		},
	}
	if errs := ValidateProjectDeclarations(projects); len(errs) != 0 {
		t.Fatalf("valid project rejected: %v", errs)
	}

	projects["acme"].Features = append(projects["acme"].Features, "not-registered")
	errs := ValidateProjectDeclarations(projects)
	if errs["acme"] == nil || !strings.Contains(errs["acme"].Error(), "not registered") {
		t.Fatalf("missing project feature error = %v", errs["acme"])
	}
}

func TestNormalizeFeatureNames(t *testing.T) {
	got, err := NormalizeFeatureNames([]string{" Records ", "menu", "MENU", "records"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"records", "menu"}
	if len(got) != len(want) {
		t.Fatalf("normalized = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("normalized = %v, want %v", got, want)
		}
	}
	if _, err := NormalizeFeatureNames([]string{"records", "  "}); err == nil {
		t.Fatal("empty feature name accepted")
	}
}

func TestUnmanifestedProjects(t *testing.T) {
	resetProjectRegistryForTest()
	RegisterProject("with-manifest", "sports", Feature{Name: "facilities"})
	RegisterProject("no-manifest", "sports", Feature{Name: "facilities"})
	RegisterProject("load-error", "sports", Feature{Name: "facilities"})

	projects := map[string]*agentconfig.Project{
		"with-manifest": {Slug: "with-manifest", Vertical: "sports"},
	}
	bad := map[string]error{"load-error": errors.New("bad project.json")}
	got := UnmanifestedProjects(projects, bad)
	if len(got) != 1 || got[0] != "no-manifest" {
		t.Fatalf("unmanifested = %v, want [no-manifest]", got)
	}
}
