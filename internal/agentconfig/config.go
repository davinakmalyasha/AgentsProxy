// Package agentconfig loads and validates the per-tenant agent
// configuration that lives in tenants/<slug>/:
//
//	tools/    tool implementations (Go, compiled)
//	skills/   procedures (markdown, always injected)
//	memory/   prompt, knowledge, identity (markdown + JSON)
//	backend/  the tenant's app backend (Go, compiled)
//
// The split this package enforces:
//
//	declaration -> data      prompts, skills, knowledge
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
	// SystemPrompt is the tenant-level persona and house rules.
	SystemPrompt string
	// Information is the long-lived knowledge the agent may state as fact:
	// price bands, opening hours, policies.
	Information []Document
	// Skills are procedures -- how to close the day, how to handle a
	// cancellation. Every skill is injected whole, every turn.
	Skills []Skill
}

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
// surface as a broken agent rather than a failed boot: bad slug, missing
// vertical, missing/empty skills, blown size budgets.
func Validate(p *Project, b Budget) error {
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

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("project %q: %s", p.Slug, strings.Join(errs, "; "))
}
