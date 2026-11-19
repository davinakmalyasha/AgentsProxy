// Package features is the Phase 3 feature platform.
//
// DONE: PRD 6.5 (feature platform) / STANDARDS A, B, F
package features

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
)

// Feature is the unit of capability a client may enable. A feature
// is registered at boot and is immutable thereafter.
type Feature struct {
	// Name is unique, lowercase, short — e.g. "menu".
	Name string

	// Kind is the value written to tenant_records.kind. It is set
	// server-side by the feature adapter; clients cannot fake it.
	// Empty means the feature does not own a kind (e.g. a route-only
	// feature).
	Kind string

	// VerticalSlugs is the allow-list of verticals that can use this
	// feature. Empty means "all allowed verticals".
	VerticalSlugs []string

	// Routes is called once per vertical at build time with the
	// builder's deps (pool + cache). Nil = no custom HTTP routes
	// (the feature is just a validation contract over
	// tenant_records, served by the records adapter).
	Routes func(chi.Router, BuilderDeps)

	// PublicRead marks a project feature whose GET routes are safe for
	// the unauthenticated public landing read (facilities, pricing,
	// availability). It has no effect on the legacy global registry.
	PublicRead bool

	// ValidatePayload runs on POST/PATCH. Nil = accept anything.
	ValidatePayload func(json.RawMessage) error
}

var (
	mu        sync.RWMutex
	all       = map[string]Feature{}
	seenNames = map[string]bool{} // for the duplicate-name panic on Register
)

// Register adds a Feature to the process registry. Must be called
// exactly once per Feature at boot. Panics on duplicate name (case-
// insensitive) so misconfiguration dies at startup, not in prod.
func Register(f Feature) {
	mu.Lock()
	defer mu.Unlock()
	name := strings.ToLower(strings.TrimSpace(f.Name))
	if name == "" {
		panic("features.Register: empty feature name")
	}
	if seenNames[name] {
		panic(fmt.Sprintf("features.Register: duplicate feature name %q", name))
	}
	seenNames[name] = true
	f.Name = name
	f.Kind = strings.ToLower(strings.TrimSpace(f.Kind))
	if len(f.VerticalSlugs) == 0 {
		f.VerticalSlugs = nil // explicit
	}
	all[name] = f
}

// MustInit resets the registry. Intended for tests; in production the
// init() in main.go is sufficient.
func MustInit() {
	mu.Lock()
	defer mu.Unlock()
	all = map[string]Feature{}
	seenNames = map[string]bool{}
}

// All returns a stable, sorted slice of every registered feature.
func All() []Feature {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Feature, 0, len(all))
	for _, f := range all {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the Feature with the given name (case-insensitive).
func Get(name string) (Feature, bool) {
	mu.RLock()
	defer mu.RUnlock()
	f, ok := all[strings.ToLower(strings.TrimSpace(name))]
	return f, ok
}

// AllowedForVertical reports whether the named feature is permitted
// for the given vertical. A feature with an empty VerticalSlugs is
// permitted for every vertical; otherwise the vertical must be in
// the list. The implicit "records" feature is always allowed.
func AllowedForVertical(name, vertical string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "records" {
		return true
	}
	mu.RLock()
	defer mu.RUnlock()
	f, ok := all[name]
	if !ok {
		return false
	}
	if len(f.VerticalSlugs) == 0 {
		return true
	}
	for _, v := range f.VerticalSlugs {
		if v == vertical {
			return true
		}
	}
	return false
}

// hasFeature reports whether the given feature name is registered.
func hasFeature(name string) bool {
	mu.RLock()
	defer mu.RUnlock()
	_, ok := all[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// ---------------------------------------------------------------------------
// Project-scoped features
// ---------------------------------------------------------------------------
//
// Platform features (records, booking, menu, ...) live in the global
// registry above and are shared by every tenant. Project features are owned
// by exactly one project and are mounted under /v1/p/<slug>/..., so two
// projects may both implement a feature named "bookings" without colliding.
//
// The legacy Centrum backend still registers into the global registry to keep
// /v1/sports/... working unchanged. New projects use RegisterProject.

var (
	projectMu        sync.RWMutex
	projectFeatures  = map[string]map[string]Feature{}
	projectVerticals = map[string]string{}
	projectSlugRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

// RegisterProject adds a feature owned by one project. The feature name is
// scoped to the project; duplicate names for the same project panic at boot.
func RegisterProject(project, vertical string, f Feature) {
	project = strings.ToLower(strings.TrimSpace(project))
	name := strings.ToLower(strings.TrimSpace(f.Name))
	if project == "" {
		panic("features.RegisterProject: empty project")
	}
	if !projectSlugRe.MatchString(project) {
		panic("features.RegisterProject: invalid project slug " + project)
	}
	if name == "" {
		panic("features.RegisterProject: empty feature name")
	}
	projectMu.Lock()
	defer projectMu.Unlock()
	if projectFeatures[project] == nil {
		projectFeatures[project] = map[string]Feature{}
	}
	if _, exists := projectFeatures[project][name]; exists {
		panic("features.RegisterProject: duplicate feature " + project + "/" + name)
	}
	f.Name = name
	f.Kind = strings.ToLower(strings.TrimSpace(f.Kind))
	if len(f.VerticalSlugs) == 0 {
		f.VerticalSlugs = nil
	}
	projectFeatures[project][name] = f
	if vertical != "" {
		projectVerticals[project] = strings.ToLower(strings.TrimSpace(vertical))
	}
}

// ProjectFeatures returns a stable, sorted slice of one project's features.
func ProjectFeatures(project string) []Feature {
	project = strings.ToLower(strings.TrimSpace(project))
	projectMu.RLock()
	defer projectMu.RUnlock()
	m := projectFeatures[project]
	out := make([]Feature, 0, len(m))
	for _, f := range m {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ProjectVertical returns the vertical registered for a project.
func ProjectVertical(project string) string {
	project = strings.ToLower(strings.TrimSpace(project))
	projectMu.RLock()
	defer projectMu.RUnlock()
	return projectVerticals[project]
}

// RegisteredProjects returns every project that registered at least one
// feature, sorted.
func RegisteredProjects() []string {
	projectMu.RLock()
	defer projectMu.RUnlock()
	out := make([]string, 0, len(projectFeatures))
	for p := range projectFeatures {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// resetProjectRegistryForTest clears project-scoped registrations. Test-only.
func resetProjectRegistryForTest() {
	projectMu.Lock()
	defer projectMu.Unlock()
	projectFeatures = map[string]map[string]Feature{}
	projectVerticals = map[string]string{}
}
