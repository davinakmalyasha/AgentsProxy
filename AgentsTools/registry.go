// Package agentstools is the tool registry: every tool the agent can call.
//
// Each tool runs against the tenant's data through WithTenantTx (RLS) -- the
// SAME path the REST API uses, so double-booking, stock guards, and tenant
// isolation are inherited.
//
// Tools self-register from their own packages via init(), the same pattern
// internal/features uses:
//
//	func init() { agentstools.Register("centrum", "list_bookings", listBookingsTool) }
//
// The tenant is part of the key: two tenants may each implement
// "book_slot" without colliding, and no tenant ever sees another
// tenant's tools. A constructor takes the pool and returns a Tool, so
// the registry holds no connections and tests build fresh sets without
// global state leaking between them. adkrunner builds role-filtered
// views on top.
package agentstools

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Tool is one agent capability.
type Tool struct {
	Name        string
	Description string
	// Parameters is a JSON Schema for the tool's arguments. It travels with
	// the declaration to the provider, so the model has a real contract
	// instead of an English sentence in Description, and the agent can
	// reject a bad call before it reaches SQL. nil is treated as "no
	// arguments".
	Parameters json.RawMessage
	Run        func(ctx context.Context, tenantID string, args json.RawMessage) string
	// Roles that may call this tool. Empty = all (legacy). Used for isolation.
	Roles []string
	// Tenant is the slug that registered this tool, stamped at build time
	// from the registry key -- never set by hand. Empty means global: a
	// hand-built set (tests) is visible to every tenant.
	Tenant string
	// Mutating marks a tool that CHANGES state -- creates, cancels,
	// adjusts, records. Reads are not.
	//
	// This is what keeps a tenant config honest: ApplyProject may narrow
	// a role's tool set but can never reclassify a write as a read, and
	// boot validation fails a project that tries. A user asking about
	// revenue must never have a booking cancelled because the model
	// volunteered it, and the tiers are built so a role that should not
	// write never sees the tool at all.
	//
	// Defaults to false, so a new read-only tool is safe by omission. The
	// cost of forgetting it on a WRITE tool is the tool being usable where
	// it should not be, so TestMutatingToolsAreExplicitlyClassified fails
	// the build if a tool is added to that list without the flag.
	Mutating bool
}

// Constructor builds one tool against a pool. Registered by name; called
// once per ToolSet build so every instance closes over the pool it runs on.
type Constructor func(pool *pgxpool.Pool) Tool

// tenantTool keys the registry: (tenant, name), so tenants never collide.
type tenantTool struct {
	tenant string
	name   string
}

var registry = map[tenantTool]Constructor{}

// Register adds a tool constructor under (tenant, name). Called from init()
// in the tool's own package. A duplicate fails the build loudly rather
// than letting one tool silently shadow another.
func Register(tenant, name string, fn Constructor) {
	k := tenantTool{tenant: tenant, name: name}
	if _, exists := registry[k]; exists {
		panic("agentstools: duplicate tool registration for " + tenant + "/" + name)
	}
	registry[k] = fn
}

// Tenants returns every slug with at least one registered tool, sorted.
// A tenant directory without tools is a configuration error the boot
// validation catches, not a silent agent.
func Tenants() []string {
	seen := map[string]bool{}
	for k := range registry {
		seen[k.tenant] = true
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// ToolSet holds the tools for one agent instance.
type ToolSet struct {
	tools map[string]Tool
	Pool  *pgxpool.Pool
}

// NewToolSetFrom builds a set from explicit tools rather than the global
// registry. Tests use it to combine real metadata with stub execution;
// production uses NewToolSetFor, which reads the registry populated by init().
func NewToolSetFrom(pool *pgxpool.Pool, tools ...Tool) *ToolSet {
	ts := &ToolSet{tools: make(map[string]Tool, len(tools)), Pool: pool}
	for _, t := range tools {
		ts.tools[t.Name] = t
	}
	return ts
}

// AllTools runs every registered constructor and returns the stamped tools
// as a list, sorted by (tenant, name). A list rather than a set because
// two tenants may implement the same tool name -- a name-keyed map would
// silently drop one. Used for boot-time checks that must see everything
// (schema parse proofs, config validation grouping, MCP registration).
func AllTools(pool *pgxpool.Pool) []Tool {
	keys := make([]tenantTool, 0, len(registry))
	for k := range registry {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].tenant != keys[j].tenant {
			return keys[i].tenant < keys[j].tenant
		}
		return keys[i].name < keys[j].name
	})
	out := make([]Tool, 0, len(keys))
	for _, k := range keys {
		t := registry[k](pool)
		t.Tenant = k.tenant
		out = append(out, t)
	}
	return out
}

// pegawai, owner. Single shared LLM key (AgentsProxy/.env LLM_API_KEY) is
// reused, isolation is by allowlist -- no role sees a tool it was not
// granted, even if the model guesses the name.
//
// "anon" exists because the public landing chat has no credential but
// still runs tools against a caller-supplied tenant; "client" is the
// authenticated API/agent tier and keeps its data tools.
//
// tenant selects the registering tenant. Empty means all tenants -- used
// only where a global view is needed (boot checks iterate AllTools
// instead); production callers always pass a concrete tenant, because a
// name-keyed set cannot hold two tenants' same-named tools at once.
func NewToolSetFor(pool *pgxpool.Pool, tenant, role string) *ToolSet {
	ts := &ToolSet{tools: map[string]Tool{}, Pool: pool}
	for _, t := range AllTools(pool) {
		if tenant != "" && t.Tenant != "" && t.Tenant != tenant {
			continue
		}
		if role == "owner" {
			ts.tools[t.Name] = applyToolMeta(t)
			continue
		}
		if AllowedToolsFor(role)[t.Name] {
			ts.tools[t.Name] = applyToolMeta(t)
		}
	}
	return ts
}

// ForTenant narrows a hand-built set to one tenant's tools, keeping
// globally-stamped (empty Tenant) tools visible to all. Tests use this
// to assert isolation without touching the global registry.
func (ts *ToolSet) ForTenant(tenant string) *ToolSet {
	out := &ToolSet{tools: map[string]Tool{}, Pool: ts.Pool}
	for name, t := range ts.tools {
		if t.Tenant == "" || t.Tenant == tenant {
			out.tools[name] = t
		}
	}
	return out
}

func AllowedToolsFor(role string) map[string]bool {
	switch role {
	case "anon":
		// PUBLIC LANDING WIDGET -- no credential of any kind.
		//
		// /v1/agent/client-chat accepts a caller-supplied tenant_id, so
		// this tier must not be able to read tenant data at all: the
		// caller chooses which tenant the tools run against. Anything
		// that returns rows is excluded, which is why list_bookings
		// (customer names, times, statuses) and query_attendance (visit
		// counts) are NOT here even though the "client" tier has them.
		//
		// What a public FAQ widget actually needs: can I book, what does
		// it cost, and when is it free.
		return map[string]bool{
			"get_pricing":      true,
			"get_availability": true,
		}
	case "client":
		// Authenticated API/agent integration (X-Client-Token). RLS
		// scopes these to the token's own tenant, so listing that
		// tenant's bookings is legitimate here. This tier is NOT usable
		// for a public endpoint that lets the caller pick the tenant.
		return map[string]bool{
			"check_stock":      true,
			"query_attendance": true,
			"list_bookings":    true,
			"get_pricing":      true,
			"get_availability": true,
		}
	case "pegawai":
		return map[string]bool{
			"book_slot":           true,
			"cancel_booking":      true,
			"list_bookings":       true,
			"query_income":        true,
			"query_attendance":    true,
			"check_stock":         true,
			"adjust_stock":        true,
			"set_reminder":        true,
			"closing_report":      true, // daily only
			"list_staff_payments": true,
			"get_pricing":         true,
			"get_availability":    true,
		}
	case "owner":
		return nil // handled above
	default:
		return map[string]bool{}
	}
}

// Get returns a tool by name.
func (ts *ToolSet) Get(name string) (Tool, bool) {
	t, ok := ts.tools[name]
	return t, ok
}

// Names returns the available tool names, sorted.
func (ts *ToolSet) Names() []string {
	names := make([]string, 0, len(ts.tools))
	for n := range ts.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Definitions returns the tool list for the system prompt.
func (ts *ToolSet) Definitions() []string {
	// Sort for a stable prompt (map order is random per run).
	names := make([]string, 0, len(ts.tools))
	for n := range ts.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	defs := make([]string, 0, len(ts.tools))
	for _, n := range names {
		defs = append(defs, ts.tools[n].Name+": "+ts.tools[n].Description)
	}
	return defs
}
