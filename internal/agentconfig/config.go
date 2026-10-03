// Package agentconfig loads and validates the per-tenant agent
// configuration that lives in tenants/<slug>/:
//
//	tools/    tool implementations (Go, compiled)
//	skills/   procedures (markdown, always injected)
//	mcp/      external MCP servers (JSON)
//	memory/   prompt, knowledge, identity (markdown + JSON)
//	backend/  the tenant's app backend (Go, compiled)
//
// The split this package enforces:
//
//	declaration -> data      prompts, skills, knowledge, MCP servers
//	implementation -> code   the Run functions in tenants/<slug>/tools
//	                        and the feature handlers in tenants/<slug>/backend
//
// A tenant folder can therefore change what its agent knows without a
// rebuild, but it can never introduce new behaviour or a new SQL statement.
// That asymmetry is deliberate: the tenant guarantee lives in the
// implementations (tenantID as the first argument, WHERE tenant_id, and RLS
// beneath both), so making those pluggable from a file would be a security
// downgrade.
//
// Nothing here is read at request time. The whole tree is embedded at build
// time and parsed once at boot, so a typo fails at startup rather than
// turning into an agent with no tools at 3am.
package agentconfig

import (
	"fmt"
	"strings"
)

// Project is one tenant's agent configuration.
type Project struct {
	// Slug is the tenant id it serves, from tenants/<dir>/memory/project.json.
	// Lowercase, no spaces: it is an identifier, not a label.
	Slug string
	// Display is the human name, used in logs and the MCP description.
	Display string
	// Contact is the owner contact quoted in refusal and quota replies
	// (client tier). Optional; empty falls back to the platform default.
	// It lives here rather than in SystemPrompt.md so shared code can
	// address the tenant without parsing prose.
	Contact string
	// Vertical decides which feature set the tenant's tools come from.
	Vertical string
	// Tools is reserved for per-tenant narrowing and is currently NOT loaded
	// from files: tool declarations live in tenants/<slug>/tools/*.go next
	// to their implementations, because the schema must travel with the
	// function it describes. See ApplyProject in AgentsTools (projects.go).
	Tools []ToolSpec
	// SystemPrompt is the tenant-level persona and house rules.
	SystemPrompt string
	// Information is the long-lived knowledge the agent may state as fact:
	// price bands, opening hours, policies.
	Information []Document
	// Skills are procedures -- how to close the day, how to handle a
	// cancellation. Every skill is injected whole, every turn.
	Skills []Skill
	// MCP names external MCP servers this tenant connects to.
	MCP []MCPServer
}

// ToolSpec is the shape of a per-tenant tool override. It is NOT loaded
// from files today: tool declarations live in tenants/<slug>/tools/*.go
// next to their implementations, so the schema travels with the function.
// The type stays because the gate and ApplyProject reason about tools in
// these terms, and a future narrowing file would populate exactly this.
type ToolSpec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Roles narrows the tool for this tenant. It can only REMOVE a role
	// from the platform ceiling, never add one -- see Validate.
	Roles []string `json:"roles,omitempty"`
	// Mutating must match the Go flag on the implementation.
	Mutating *bool `json:"mutating,omitempty"`
}

// IsEnabled reports whether the spec is active. Absent means enabled.
func (t ToolSpec) IsEnabled() bool { return true }

// Document is one file under Information/.
type Document struct {
	// Name is the file's base name, used for logging and for the budget.
	Name string
	Body string
}

// Skill is one .md file under tenants/<slug>/skills/. Every skill is
// injected whole, every turn: a tenant's skills file is the procedures it
// wants followed, and a procedure injected only sometimes is followed only
// sometimes. If the file ever grows noisy enough that always-on hurts, split
// it -- do not add a matching layer on top.
type Skill struct {
	// Name is the file's base name.
	Name string
	// Title is the first "# " heading, or the file name. It is what the
	// assembled prompt calls the procedure.
	Title string
	Body  string
}

// MCPServer is an external MCP server this project connects to.
type MCPServer struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Transport is "http" or "stdio". Only http is implemented.
	Transport string `json:"transport"`
	// ReadOnly refuses to expose any tool from this server that is not
	// declared read-only upstream. Default true, because an MCP server is
	// third-party code and this agent runs against a live business.
	ReadOnly *bool `json:"read_only,omitempty"`
}

// IsReadOnly reports the effective setting; absent means true.
func (m MCPServer) IsReadOnly() bool { return m.ReadOnly == nil || *m.ReadOnly }

// RoleCeiling is the platform's answer to "who may call this", independent
// of any project. agentconfig can only narrow it.
type RoleCeiling map[string]map[string]bool

// Valid reports whether the role may call the tool under the ceiling.
func (c RoleCeiling) Valid(role, tool string) bool {
	allowed, ok := c[role]
	if !ok {
		return false
	}
	return allowed[tool]
}

// Budget is the per-layer size ceiling. These strings go straight into a
// provider prompt on every turn, so an unbounded file is an unbounded bill
// and a context window nobody can debug.
type Budget struct {
	MaxSystemPromptRunes int
	MaxInformationRunes  int
	MaxSkillRunes        int
	MaxTotalRunes        int
}

// DefaultBudget is deliberately generous but finite.
func DefaultBudget() Budget {
	return Budget{
		MaxSystemPromptRunes: 8_000,
		MaxInformationRunes:  32_000,
		MaxSkillRunes:        6_000,
		MaxTotalRunes:        64_000,
	}
}

// Validate checks the project for the mistakes that would otherwise
// surface as a broken agent rather than a failed boot.
//
// `known` is the set of tool names the Go registry actually provides, and
// `mutating` their flags. Both are required: drift in either direction is
// the bug, and a config that names a tool nobody implements is the exact
// failure that leaves an owner staring at an agent that knows nothing.
func Validate(p *Project, known map[string]bool, mutating map[string]bool, ceiling RoleCeiling, b Budget) error {
	var errs []string
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	if strings.TrimSpace(p.Slug) == "" {
		add("slug is required")
	} else if p.Slug != strings.ToLower(p.Slug) || strings.ContainsAny(p.Slug, " /\\") {
		add("slug %q must be lowercase and contain no spaces or path separators", p.Slug)
	}
	if p.Vertical == "" {
		add("vertical is required")
	}

	seen := map[string]bool{}
	for _, t := range p.Tools {
		if t.Name == "" {
			add("a tool entry has no name")
			continue
		}
		if seen[t.Name] {
			add("tool %q is declared twice", t.Name)
		}
		seen[t.Name] = true
		if !known[t.Name] {
			add("tool %q is declared but not implemented in the registry; "+
				"a config typo must fail at boot, not silently disable the tool", t.Name)
			continue
		}
		if t.Mutating != nil && *t.Mutating != mutating[t.Name] {
			add("tool %q declares mutating=%v but the implementation is mutating=%v; "+
				"the gate trusts the Go flag, so a mismatch here would be a lie in the config",
				t.Name, *t.Mutating, mutating[t.Name])
		}
		// Narrow only. A tenant may remove a role from a tool's platform
		// ceiling; it may never add one, because that is privilege
		// escalation expressed as a data file.
		if len(t.Roles) > 0 {
			for _, r := range t.Roles {
				if !ceiling.Valid(r, t.Name) {
					add("tool %q grants role %q, which the platform does not allow; "+
						"a tenant config can only narrow the ceiling, never widen it", t.Name, r)
				}
			}
		}
	}

	for _, s := range p.Skills {
		if s.Name == "" {
			add("a skill has no name")
			continue
		}
		if strings.TrimSpace(s.Body) == "" {
			add("skill %q has no body", s.Name)
		}
		if n := len([]rune(s.Body)); n > b.MaxSkillRunes {
			add("skill %q is %d runes, over the %d budget", s.Name, n, b.MaxSkillRunes)
		}
	}

	info := 0
	for _, d := range p.Information {
		info += len([]rune(d.Body))
	}
	if info > b.MaxInformationRunes {
		add("Information/ totals %d runes, over the %d budget; "+
			"at this size it wants retrieval, not prompt injection", info, b.MaxInformationRunes)
	}
	if n := len([]rune(p.SystemPrompt)); n > b.MaxSystemPromptRunes {
		add("SystemPrompt.md is %d runes, over the %d budget", n, b.MaxSystemPromptRunes)
	}

	for _, m := range p.MCP {
		if m.Name == "" || m.URL == "" {
			add("an mcp server entry needs both name and url")
			continue
		}
		switch strings.ToLower(m.Transport) {
		case "", "http":
		case "stdio":
			add("mcp server %q uses stdio transport, which is not implemented", m.Name)
		default:
			add("mcp server %q has unknown transport %q", m.Name, m.Transport)
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("project %q: %s", p.Slug, strings.Join(errs, "; "))
}
