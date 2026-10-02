package agentstools

import (
	"fmt"
	"sort"

	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
)

// LoadProjects parses the embedded tenant configuration and validates it
// against the live tool registry.
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
func LoadProjects(tools *ToolSet) (map[string]*agentconfig.Project, error) {
	projects, err := agentconfig.Load()
	if err != nil {
		return nil, fmt.Errorf("load agent projects: %w", err)
	}

	known := map[string]bool{}
	mutating := map[string]bool{}
	for _, name := range tools.Names() {
		known[name] = true
		mutating[name] = tools.tools[name].Mutating
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

	budget := agentconfig.DefaultBudget()
	slugs := make([]string, 0, len(projects))
	for slug := range projects {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	for _, slug := range slugs {
		if err := agentconfig.Validate(projects[slug], known, mutating, ceiling, budget); err != nil {
			return nil, err
		}
	}
	return projects, nil
}

// ApplyProject narrows a role tool set to what a tenant's config allows.
//
// Today the loader populates no per-tool specs -- tool declarations live in
// AgentsTools/<Tenant>/*.go next to their implementations, because the
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
