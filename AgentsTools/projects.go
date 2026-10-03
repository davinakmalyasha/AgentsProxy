package agentstools

import (
	"fmt"
	"sort"

	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
)

// LoadProjects parses the embedded tenant configuration and validates
// each tenant against the tools ITS package registered.
//
// This runs once at boot and a failure is fatal on purpose. Every mistake it
// catches -- a tool named in a tenant file that nobody implements, a role a
// tenant tried to grant itself, a skill file that is missing -- produces an
// agent that looks configured and quietly cannot do its job. Finding out at
// startup costs a failed deploy; finding out at 3am costs the owner a
// conversation with a bot that says it cannot reach the system.
//
// Validation lives here rather than in main because it needs the registry,
// the Mutating flags and the role ceiling. main just gets a map or an error.
//
// tools is the flat list from AllTools: every stamped tool from every
// tenant. Each project is checked against its own tenant's tools, so a
// tenant that registers nothing fails here -- an agent with no tools is a
// configuration error, not a silent agent.
func LoadProjects(all []Tool) (map[string]*agentconfig.Project, error) {
	projects, err := agentconfig.Load()
	if err != nil {
		return nil, fmt.Errorf("load agent projects: %w", err)
	}

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
		mutating := map[string]bool{}
		for _, t := range byTenant[slug] {
			known[t.Name] = true
			mutating[t.Name] = t.Mutating
		}
		if len(known) == 0 {
			return nil, fmt.Errorf("tenant %q has no tools registered; an agent with no tools cannot serve", slug)
		}

		// The platform ceiling, straight from allowedToolsFor. A tenant config
		// is checked against this and may only narrow it.
		ceiling := agentconfig.RoleCeiling{}
		for _, role := range []string{"anon", "client", "pegawai", "owner"} {
			allowed := map[string]bool{}
			for name := range known {
				if AllowedToolsFor(role)[name] {
					allowed[name] = true
				}
			}
			ceiling[role] = allowed
		}

		if err := agentconfig.Validate(projects[slug], known, mutating, ceiling, budget); err != nil {
			return nil, err
		}
	}
	return projects, nil
}

// ApplyProject narrows a role tool set to what a tenant's config allows.
//
// Today the loader populates no per-tool specs -- tool declarations live in
// tenants/<slug>/tools/*.go next to their implementations, because the
// schema must travel with the function it describes. So in production this
// is currently a passthrough, and tool selection is structural: a tenant
// gets the tools its package registers.
//
// The narrowing logic stays because it is the enforcement point for the day
// a per-tenant override file exists again: intersect, never add. A spec that
// widens access is a no-op here and a boot failure in Validate, so neither
// path can escalate privilege by forgetting.
func (ts *ToolSet) ApplyProject(p *agentconfig.Project) *ToolSet {
	if p == nil || len(p.Tools) == 0 {
		return ts
	}
	enabled := map[string]bool{}
	params := map[string]agentconfig.ToolSpec{}
	for _, spec := range p.Tools {
		if !spec.IsEnabled() {
			continue
		}
		enabled[spec.Name] = true
		params[spec.Name] = spec
	}

	out := &ToolSet{Pool: ts.Pool, tools: make(map[string]Tool, len(ts.tools))}
	for name, tool := range ts.tools {
		if !enabled[name] {
			continue
		}
		// Narrowing only: intersect, never add.
		if len(params[name].Roles) > 0 {
			keep := map[string]bool{}
			for _, r := range params[name].Roles {
				keep[r] = true
			}
			var kept []string
			for _, r := range tool.Roles {
				if keep[r] {
					kept = append(kept, r)
				}
			}
			tool.Roles = kept
		}
		out.tools[name] = tool
	}
	return out
}
