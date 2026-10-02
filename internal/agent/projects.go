package agent

import (
	"fmt"
	"sort"

	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
)

// LoadProjects parses the embedded project configuration and validates it
// against the live tool registry.
//
// This runs once at boot and a failure is fatal on purpose. Every mistake it
// catches -- a tool named in tools.json that nobody implements, a role a
// project tried to grant itself, a skill whose file is missing -- produces
// an agent that looks configured and quietly cannot do its job. Finding out
// at startup costs a failed deploy; finding out at 3am costs the owner a
// conversation with a bot that says it cannot reach the system.
//
// Validation is done here rather than in main because it needs the registry,
// the Mutating flags and the role ceiling, all of which live in this
// package. main just gets a map or an error.
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

	// The platform ceiling, straight from allowedToolsFor. A project config
	// is checked against this and may only narrow it.
	ceiling := agentconfig.RoleCeiling{}
	for _, role := range []string{"anon", "client", "pegawai", "owner"} {
		allowed := map[string]bool{}
		for name := range known {
			if allowedToolsFor(role)[name] {
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

// ApplyProject narrows the tool set to what a project actually enables, and
// folds in that project's keyword tables.
//
// Two things happen here, and the second is the reason this is not just a
// filter:
//
//  1. Enabled tools are selected, so a project can say "this venue does not
//     use set_reminder" without touching Go.
//  2. Keywords and schemas come from the PROJECT, not from the compiled
//     table. That is what makes the project folder the source of truth for
//     routing -- and it is why a project can have "pesanan" reach
//     list_bookings while book_slot only matches the verb.
//
// The Mutating flag is deliberately NOT taken from the project. The gate
// trusts the Go flag; letting a config file decide it would let a project
// reclassify a write as a read and have unpinned writes honoured.
//
// Roles from the project can only subtract from what the role's tool set
// already contains, so a config that widens access is a no-op rather than a
// privilege escalation. Validate rejects it at boot as well, but failing
// safe here means a future caller cannot introduce one by forgetting.
func (ts *ToolSet) ApplyProject(p *agentconfig.Project) *ToolSet {
	if p == nil || len(p.Tools) == 0 {
		return ts
	}
	enabled := map[string]bool{}
	keywords := map[string][]string{}
	strong := map[string][]string{}
	params := map[string]agentconfig.ToolSpec{}
	for _, spec := range p.Tools {
		if !spec.IsEnabled() {
			continue
		}
		enabled[spec.Name] = true
		params[spec.Name] = spec
		if len(spec.Keywords) > 0 {
			keywords[spec.Name] = spec.Keywords
		}
		if len(spec.Strong) > 0 {
			strong[spec.Name] = spec.Strong
		}
	}

	out := &ToolSet{Pool: ts.Pool, tools: make(map[string]Tool, len(ts.tools))}
	for name, tool := range ts.tools {
		if !enabled[name] {
			continue
		}
		if k, ok := keywords[name]; ok {
			tool.Keywords = k
		}
		if s, ok := strong[name]; ok {
			tool.StrongKeywords = s
		}
		if spec, ok := params[name]; ok {
			if len(spec.Parameters) > 0 {
				tool.Parameters = spec.Parameters
			}
			if spec.Description != "" {
				tool.Description = spec.Description
			}
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
