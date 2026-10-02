// Package agentstools is the tool registry: every tool the agent can call.
//
// Each tool runs against the tenant's data through WithTenantTx (RLS) -- the
// SAME path the REST API uses, so double-booking, stock guards, and tenant
// isolation are inherited.
//
// Tools self-register from their own packages via init(), the same pattern
// internal/features uses:
//
//	func init() { agentstools.Register("list_bookings", listBookingsTool) }
//
// A constructor takes the pool and returns a Tool, so the registry holds no
// connections and tests build fresh sets without global state leaking
// between them. internal/agent builds role-filtered views on top.
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

var registry = map[string]Constructor{}

// Register adds a tool constructor under its name. Called from init() in the
// tool's own package. A duplicate name fails the build loudly rather than
// letting one tool silently shadow another.
func Register(name string, fn Constructor) {
	if _, exists := registry[name]; exists {
		panic("agentstools: duplicate tool registration for " + name)
	}
	registry[name] = fn
}

// ToolSet holds the tools for one agent instance.
type ToolSet struct {
	tools map[string]Tool
	Pool  *pgxpool.Pool
}

// NewToolSet builds the full set: every registered constructor, run against
// this pool. A tenant that registers no tools gets an empty set, which is a
// configuration error the boot validation catches, not a silent agent.
func NewToolSet(pool *pgxpool.Pool) *ToolSet {
	ts := &ToolSet{tools: map[string]Tool{}, Pool: pool}
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ts.add(registry[n](pool))
	}
	return ts
}

// NewToolSetFrom builds a set from explicit tools rather than the global
// registry. Tests use it to combine real metadata with stub execution;
// production uses NewToolSet, which reads the registry populated by init().
func NewToolSetFrom(pool *pgxpool.Pool, tools ...Tool) *ToolSet {
	ts := &ToolSet{tools: make(map[string]Tool, len(tools)), Pool: pool}
	for _, t := range tools {
		ts.tools[t.Name] = t
	}
	return ts
}

// pegawai, owner. Single shared LLM key (AgentsProxy/.env LLM_API_KEY) is
// reused, isolation is by allowlist -- no role sees a tool it was not
// granted, even if the model guesses the name.
//
// "anon" exists because the public landing chat has no credential but
// still runs tools against a caller-supplied tenant; "client" is the
// authenticated API/agent tier and keeps its data tools.
func NewToolSetFor(pool *pgxpool.Pool, role string) *ToolSet {
	full := NewToolSet(pool)
	if role == "owner" {
		return full // all
	}
	allowed := AllowedToolsFor(role)
	filtered := &ToolSet{tools: map[string]Tool{}, Pool: pool}
	for name, t := range full.tools {
		if allowed[name] {
			filtered.tools[name] = t
		}
	}
	return filtered
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

func (ts *ToolSet) add(t Tool) { ts.tools[t.Name] = applyToolMeta(t) }

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
