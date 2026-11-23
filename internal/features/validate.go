package features

import (
	"fmt"
	"strings"

	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
)

// NormalizeFeatureNames canonicalizes a requested entitlement set. Feature
// names are identifiers, not display text: the registry lowercases on
// registration, so accepting "Menu" here would store a value the exact-match
// gate never sees. Duplicates are collapsed in first-seen order. An empty
// name is refused rather than dropped, because it usually means a client
// sent a trailing comma or an unset field.
func NormalizeFeatureNames(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			return nil, fmt.Errorf("feature name is empty")
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, nil
}

// ValidateProjectDeclarations checks each project.json feature list against
// the compiled project registry and the project's own vertical. This is the
// boot-side counterpart to migrate check: a typo or a feature whose vertical
// changed must fail before the project is mounted.
func ValidateProjectDeclarations(projects map[string]*agentconfig.Project) map[string]error {
	out := map[string]error{}
	for slug, p := range projects {
		for _, name := range p.Features {
			if name == "records" {
				continue
			}
			if !IsKnownProjectFeature(slug, name) {
				out[slug] = fmt.Errorf("project.json feature %q is not registered in the project registry", name)
				break
			}
			if !ProjectFeatureAllowed(slug, name, p.Vertical) {
				out[slug] = fmt.Errorf("project.json feature %q is not allowed for vertical %q", name, p.Vertical)
				break
			}
		}
	}
	return out
}

// UnmanifestedProjects lists compiled project registries that have no
// projects/<slug>/project.json. A project already in badLoad is skipped:
// it has a concrete load error and is fenced with that reason, and calling
// it "missing" would hide the real cause. RegisteredProjects is sorted, so
// the result is stable for logs and tests.
func UnmanifestedProjects(projects map[string]*agentconfig.Project, badLoad map[string]error) []string {
	var out []string
	for _, slug := range RegisteredProjects() {
		if _, ok := projects[slug]; ok {
			continue
		}
		if _, bad := badLoad[slug]; bad {
			continue
		}
		out = append(out, slug)
	}
	return out
}
