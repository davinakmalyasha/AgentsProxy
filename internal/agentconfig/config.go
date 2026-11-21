// Package agentconfig loads and validates the per-project agent
// configuration that lives in projects/<slug>/:
//
//	project.json   identity, schema, feature and agent settings
//	agent/         persona, skills, MCP config (declarative data)
//	backend/       project HTTP features (Go, compiled)
//	agent/tools/   project-specific agent tools (Go, compiled)
//	migrations/    project schema migrations (SQL, applied per project)
//
// The split this package enforces:
//
//	declaration -> data      persona, skills, MCP servers
//	implementation -> code   project backend handlers and tools
//
// A project folder can therefore change what its agent knows without a
// rebuild, but it can never introduce new behaviour or a new SQL statement.
// That asymmetry is deliberate: the project guarantee lives in the
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
	"net/url"
	"regexp"
	"strings"
)

// AllowedVerticals is the single source for the supported business
// verticals. The feature builder consumes it; project validation rejects a
// folder that names a vertical nobody mounts.
var AllowedVerticals = []string{
	"fnb", "salon", "laundry", "klinik", "gym", "bimbel",
	"bengkel", "beauty", "travel", "property", "katering", "photo",
	"sports",
}

// IsAllowedVertical reports whether v is one of the supported verticals.
func IsAllowedVertical(v string) bool {
	for _, a := range AllowedVerticals {
		if a == v {
			return true
		}
	}
	return false
}

// Project is one client project's configuration. A project and a tenant
// are the same identity: project.json's slug is clients.tenant_id.
type Project struct {
	// Slug is the tenant id it serves, from projects/<dir>/project.json.
	// Lowercase, no spaces: it is an identifier, not a label.
	Slug string
	// Display is the human name, used in logs and the MCP description.
	Display string
	// Contact is the owner contact quoted in refusal and quota replies
	// (client tier). Optional; empty falls back to the platform default.
	Contact string
	// Vertical decides which feature set the project's tools come from.
	Vertical string
	// Schema is the Postgres schema that owns this project's business
	// tables. Every project, including Centrum, uses "project_<slug>"; the
	// runtime sets search_path from this value.
	Schema string
	// SystemPrompt is the project-level persona and house rules, loaded
	// from agent/agent.md.
	SystemPrompt string
	// Information is retained for prompt compatibility. New projects put
	// business facts in the database and reach them through tools, so
	// this is normally empty.
	Information []Document
	// Skills are procedures -- how to close the day, how to handle a
	// cancellation. Every skill is injected whole, every turn.
	Skills []Skill
	// Agent is the project's agent configuration.
	Agent AgentConfig
}

// AgentConfig is the declarative half of a project's agent.
type AgentConfig struct {
	// Enabled is explicit. A project can exist without an agent.
	Enabled bool `json:"enabled"`
	// Model overrides the platform model for this project (optional).
	Model string `json:"model"`
	// Tools is an optional allow-list of tool names. Empty means "all
	// tools available to the role", preserving the current behaviour.
	Tools []string `json:"tools"`
	// MCPFile is the project-relative path to the MCP server config,
	// normally agent/mcp.json.
	MCPFile string `json:"mcp"`
	// MCP lists external MCP servers this project may use. Loaded from
	// MCPFile; not serialized in project.json.
	MCP []MCPServer `json:"-"`
}

// MCPServer is one external MCP endpoint configured for a project.
//
// Headers may reference environment variables with ${NAME}; the value is
// resolved when the connection is opened, so a token never lives in
// project.json. Roles is the set of agent tiers allowed to see this server's
// tools; empty means staff only (owner + pegawai), never the public widget.
type MCPServer struct {
	Name    string            `json:"name"`
	URL     string            `json:"url"`
	Tools   []string          `json:"tools"`
	Roles   []string          `json:"roles,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Document is one knowledge document. Kept for prompt compatibility; new
// projects should prefer live data through tools over static memory.
type Document struct {
	// Name is the file's base name, used for logging and for the budget.
	Name string
	// Body is the document text.
	Body string
}

// Skill is one .md file under projects/<slug>/agent/skills/. Every skill is
// injected whole, every turn: a project's skills file is the procedures it
// wants followed, and a procedure injected only sometimes is followed only
// sometimes. If the file ever grows noisy enough that always-on hurts, split
// it -- do not add a matching layer on top.
type Skill struct {
	// Name is the file's base name.
	Name string
	// Title is the first "# " heading, or the file name. It is what the
	// assembled prompt calls the procedure.
	Title string
	// Body is the skill text.
	Body string
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
		MaxSystemPromptRunes: 16_000,
		MaxInformationRunes:  32_000,
		MaxSkillRunes:        6_000,
		MaxTotalRunes:        64_000,
	}
}

var (
	// A project schema lives in its own namespace. "public" is reserved for
	// the legacy platform tenant row (dfd-agency), which owns no project
	// folder. Keeping the prefix rule here, in the migration runner, and in
	// the admin API stops a typo like "auth" from granting app_runtime
	// access to a Supabase-managed schema.
	schemaRe     = regexp.MustCompile(`^project_[a-z0-9_]{1,55}$`)
	modelRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	mcpNameRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	headerNameRe = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
)

var mcpRoles = map[string]bool{
	"owner":   true,
	"pegawai": true,
	"client":  true,
	"anon":    true,
}

// Validate checks the project for the mistakes that would otherwise
// surface as a broken agent rather than a failed boot: bad slug, missing
// vertical, bad schema, empty agent prompt, blown budgets, invalid MCP
// servers.
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
	} else if !IsAllowedVertical(p.Vertical) {
		add("unknown vertical %q (add it to AllowedVerticals first)", p.Vertical)
	}
	if p.Schema == "" {
		add("schema is required (use project_<slug>)")
	} else if !schemaRe.MatchString(p.Schema) {
		add("schema %q must match %s (project_<slug>, max 63 characters)", p.Schema, schemaRe.String())
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
		add("Information totals %d runes, over the %d budget; "+
			"at this size it wants retrieval, not prompt injection", info, b.MaxInformationRunes)
	}
	if n := len([]rune(p.SystemPrompt)); n > b.MaxSystemPromptRunes {
		add("agent/agent.md is %d runes, over the %d budget", n, b.MaxSystemPromptRunes)
	}

	if p.Agent.Enabled {
		if strings.TrimSpace(p.SystemPrompt) == "" {
			add("agent.enabled is true but agent/agent.md is empty")
		}
		if m := strings.TrimSpace(p.Agent.Model); m != "" && !modelRe.MatchString(m) {
			add("agent.model %q is not a valid model name", m)
		}
		seenMCP := map[string]bool{}
		for _, srv := range p.Agent.MCP {
			if strings.TrimSpace(srv.Name) == "" {
				add("an MCP server has no name")
				continue
			}
			if !mcpNameRe.MatchString(srv.Name) {
				add("MCP server %q must be lowercase letters, digits, and hyphens/underscores (max 32)", srv.Name)
			}
			if seenMCP[srv.Name] {
				add("duplicate MCP server %q", srv.Name)
			}
			seenMCP[srv.Name] = true
			if srv.URL == "" {
				add("MCP server %q has no url", srv.Name)
				continue
			}
			u, err := url.Parse(srv.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				add("MCP server %q has invalid url %q", srv.Name, srv.URL)
			}
			for _, role := range srv.Roles {
				if !mcpRoles[role] {
					add("MCP server %q has unknown role %q", srv.Name, role)
				}
			}
			seenRemoteTool := map[string]bool{}
			for _, name := range srv.Tools {
				if strings.TrimSpace(name) == "" {
					add("MCP server %q has an empty tool name", srv.Name)
					continue
				}
				if seenRemoteTool[name] {
					add("MCP server %q lists tool %q twice", srv.Name, name)
				}
				seenRemoteTool[name] = true
			}
			for name, value := range srv.Headers {
				if !headerNameRe.MatchString(name) {
					add("MCP server %q has invalid header name %q", srv.Name, name)
				}
				if strings.TrimSpace(value) == "" {
					add("MCP server %q header %q is empty", srv.Name, name)
				}
			}
		}
		seenTool := map[string]bool{}
		for _, name := range p.Agent.Tools {
			if strings.TrimSpace(name) == "" {
				add("agent.tools contains an empty name")
				continue
			}
			if seenTool[name] {
				add("duplicate agent tool %q", name)
			}
			seenTool[name] = true
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("project %q: %s", p.Slug, strings.Join(errs, "; "))
}
