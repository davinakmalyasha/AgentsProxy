package agentstools

import (
	"fmt"
	"sort"
	"strings"

	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
)

// LoadProjects parses the embedded tenant configuration and validates
// each tenant against the tools ITS package registered.
//
// This runs once at boot and a failure is fatal on purpose. Every mistake it
// catches -- a skill file that is missing, a budget blown by a runaway
// prompt file -- produces an agent that looks configured and quietly cannot
// do its job. Finding out at startup costs a failed deploy; finding out at
// 3am costs the owner a conversation with a bot that says it cannot reach
// the system.
//
// Validation lives here rather than in main because it needs the registry:
// main just gets a map or an error.
//
// tools is the flat list from AllTools: every stamped tool from every
// tenant. Each project is checked against its own tenant's tools, so a
// tenant that registers nothing fails here -- an agent with no tools is a
// configuration error, not a silent agent.
func LoadProjects(all []Tool) (map[string]*agentconfig.Project, error) {
	good, bad, err := LoadProjectsCollect(all)
	if err != nil {
		return nil, err
	}
	if len(bad) > 0 {
		slugs := make([]string, 0, len(bad))
		for slug := range bad {
			slugs = append(slugs, slug)
		}
		sort.Strings(slugs)
		return nil, fmt.Errorf("invalid tenant configs: %s", strings.Join(slugs, ", "))
	}
	return good, nil
}

// LoadProjectsCollect parses and validates every tenant, collecting
// per-tenant failures instead of aborting. bad maps slug → reason and is
// what boot quarantines: those tenants are fenced while the rest serve.
// A non-nil error means the load itself failed (no tenants at all), which
// stays fatal for every tenant.
func LoadProjectsCollect(all []Tool) (good map[string]*agentconfig.Project, bad map[string]string, err error) {
	projects, badLoad := agentconfig.LoadCollect()
	bad = map[string]string{}
	for dir, lerr := range badLoad {
		bad[dir] = fmt.Errorf("load agent projects: %w", lerr).Error()
	}
	if len(projects) == 0 {
		return nil, bad, fmt.Errorf("load agent projects: no tenants")
	}
	good = projects

	// Group by registering tenant. Empty-Tenant tools (hand-built sets in
	// tests) belong to every tenant.
	byTenant := map[string][]Tool{}
	for _, t := range all {
		if t.Tenant == "" {
			for slug := range projects {
				byTenant[slug] = append(byTenant[slug], t)
			}
			continue
		}
		byTenant[t.Tenant] = append(byTenant[t.Tenant], t)
	}

	budget := agentconfig.DefaultBudget()
	slugs := make([]string, 0, len(projects))
	for slug := range projects {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	for _, slug := range slugs {
		known := map[string]bool{}
		for _, t := range byTenant[slug] {
			known[t.Name] = true
		}
		if len(known) == 0 {
			bad[slug] = fmt.Sprintf("tenant %q has no tools registered; an agent with no tools cannot serve", slug)
			delete(good, slug)
			continue
		}

		if err := agentconfig.Validate(good[slug], budget); err != nil {
			bad[slug] = err.Error()
			delete(good, slug)
		}
	}
	return good, bad, nil
}
