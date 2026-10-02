// Package agentconfig loads and validates the per-project agent
// configuration that lives in projects/<Project>/agents/.
//
// The split this package enforces:
//
//	declaration -> data      schemas, keywords, roles, prompts, skills
//	implementation -> code   the Run functions in internal/agenttools
//
// A project folder can therefore change what its agent knows and which
// tools it prefers without a rebuild, but it can never introduce new
// behaviour or a new SQL statement. That asymmetry is deliberate: the
// tenant guarantee lives in the implementations (tenantID as the first
// argument, WHERE tenant_id, and RLS beneath both), so making those
// pluggable from a file would be a security downgrade.
//
// Nothing here is read at request time. The whole tree is embedded at build
// time and parsed once at boot, so a typo fails at startup rather than
// turning into an agent with no tools at 3am.
package agentconfig

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Project is one tenant's agent configuration.
type Project struct {
	// Slug is the directory name under projects/, and the tenant id it
	// serves. Lowercase, no spaces: it is an identifier, not a label.
	Slug string
	// Display is the human name, used in logs and the MCP description.
	Display string
	// Vertical decides which feature set the project's tools come from.
	Vertical string
	// Tools declares the tools this project uses.
	Tools []ToolSpec
	// SystemPrompt is the project-level persona and house rules.
	SystemPrompt string
	// Information is the long-lived knowledge the agent may state as fact:
	// price bands, opening hours, policies.
	Information []Document
	// Skills are procedures -- how to close the day, how to handle a
	// cancellation. Injected when the message looks relevant.
	Skills []Skill
	// MCP names external MCP servers this project connects to.
	MCP []MCPServer
}

// ToolSpec is a tool's declaration. Everything here is reviewable data;
// the implementation is not.
type ToolSpec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Parameters is the JSON Schema sent to the provider.
	Parameters json.RawMessage `json:"parameters"`
	// Enabled is a pointer so a missing field means "on" rather than "off".
	// A config that forgets a tool should not silently disable it; it should
	// fail validation against the registry, which is the opposite and better
	// failure. See Validate.
	Enabled *bool `json:"enabled,omitempty"`
	// Keywords is the ordinary routing vocabulary.
	Keywords []string `json:"keywords,omitempty"`
	// Strong are decisive keywords, worth more than everything a competing
	// tool can accumulate.
	Strong []string `json:"strong,omitempty"`
	// Roles narrows the tool for this project. It can only REMOVE a role
	// from the tool's platform ceiling, never add one -- see Validate.
	Roles []string `json:"roles,omitempty"`
	// Mutating must match the Go flag on the implementation. Duplicated on
	// purpose so a project file reads as a complete description of the tool,
	// and Validate fails the build if the two disagree: a config that claims
	// a write is a read would let the gate honour it unpinned.
	Mutating *bool `json:"mutating,omitempty"`
}

// IsEnabled reports whether the spec is active. Absent means enabled.
func (t ToolSpec) IsEnabled() bool { return t.Enabled == nil || *t.Enabled }

// Document is one file under Information/.
type Document struct {
	// Name is the file's base name, used for logging and for the budget.
	Name string
	Body string
}

// Skill is one file under skills/, with the words that make it relevant.
type Skill struct {
	Name string `json:"name"`
	// Title is what the agent calls it in the prompt.
	Title string `json:"title"`
	// Triggers are matched with the same word-boundary rule the router
	// uses. A skill whose trigger words are substrings of ordinary words
	// would be injected into every conversation.
	Triggers []string `json:"triggers"`
	Body     string   `json:"-"`
	Keywords []string `json:"keywords,omitempty"`
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

// SkillsFor returns the skills whose triggers appear in the message, using
// the same word-boundary rule as the router. Injecting a skill on a
// substring hit is worse than not injecting it: the model gets instructions
// for a procedure the owner never asked about.
func (p *Project) SkillsFor(words []string) []Skill {
	var out []Skill
	for _, s := range p.Skills {
		for _, trig := range s.Triggers {
			if HasPhrase(words, trig) {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// ToolNames returns the enabled tool names, sorted for a stable prompt and
// a stable test.
func (p *Project) ToolNames() []string {
	var out []string
	for _, t := range p.Tools {
		if t.IsEnabled() {
			out = append(out, t.Name)
		}
	}
	sort.Strings(out)
	return out
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
		if len(t.Parameters) > 0 && !json.Valid(t.Parameters) {
			add("tool %q has parameters that are not valid JSON", t.Name)
		}
		if t.Mutating != nil && *t.Mutating != mutating[t.Name] {
			add("tool %q declares mutating=%v but the implementation is mutating=%v; "+
				"the gate trusts the Go flag, so a mismatch here would be a lie in the config",
				t.Name, *t.Mutating, mutating[t.Name])
		}
		// Narrow only. A project may remove a role from a tool's platform
		// ceiling; it may never add one, because that is privilege
		// escalation expressed as a data file.
		if len(t.Roles) > 0 {
			for _, r := range t.Roles {
				if !ceiling.Valid(r, t.Name) {
					add("tool %q grants role %q, which the platform does not allow; "+
						"a project config can only narrow the ceiling, never widen it", t.Name, r)
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
		if len(s.Triggers) == 0 {
			add("skill %q has no triggers, so it would never be injected; "+
				"an unconditional skill belongs in SystemPrompt.md", s.Name)
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
