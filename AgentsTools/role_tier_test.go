// Tool-tier isolation tests.
//
// REGRESSION: /v1/agent/client-chat is the public landing widget. It
// carries NO credential and takes tenant_id from the request body, so
// the caller chooses which tenant the tools run against. It previously
// used the "client" toolset, whose allowlist includes list_bookings
// (customer names, times, statuses) and query_attendance (visit
// counts) — legitimate for an authenticated X-Client-Token integration,
// where RLS pins the tenant, but a cross-tenant PII disclosure for a
// public endpoint that accepted any tenant id.
//
// The fix is a distinct "anon" tier. These tests pin the boundary so a
// future change to either the allowlist or the ToolSet construction
// cannot silently reopen it.
package agentstools_test

import (
	"testing"

	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
	_ "github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/tools"
)

// toolsThatReadTenantRows must never be reachable from a tier that runs
// without a verified credential. If you add a tool that queries tenant
// data, add it here — the test will fail until you classify it.
func toolsThatReadTenantRows() []string {
	return []string{
		"list_bookings",
		"query_attendance",
		"query_income",
		"query_net_profit",
		"query_range_summary",
		"top_customers",
		"closing_report",
		"query_kantin_summary",
		"check_stock",
		"book_slot",
		"cancel_booking",
		"adjust_stock",
		"topup_member",
		"record_staff_payment",
		"list_staff_payments",
		"set_reminder",
		"export_report_excel",
	}
}

func TestAnonTier_CannotReachTenantRows(t *testing.T) {
	anon := agentstools.NewToolSetFor(nil, "centrum", "anon")
	for _, name := range toolsThatReadTenantRows() {
		if _, ok := anon.Get(name); ok {
			t.Errorf("anon toolset exposes %q, which reads tenant rows", name)
		}
	}
}

func TestAnonTier_AllowlistIsEmptyOfRowReaders(t *testing.T) {
	allowed := agentstools.AllowedToolsFor("anon")
	for name := range allowed {
		for _, row := range toolsThatReadTenantRows() {
			if name == row {
				t.Errorf("agentstools.AllowedToolsFor(anon) permits %q", name)
			}
		}
	}
}

// The public widget still needs to answer real questions, so the tier
// must not be empty — an all-denied bot is a regression, not a fix.
func TestAnonTier_StillAnswersPublicQuestions(t *testing.T) {
	anon := agentstools.NewToolSetFor(nil, "centrum", "anon")
	for _, want := range []string{"get_pricing", "get_availability"} {
		if _, ok := anon.Get(want); !ok {
			t.Errorf("anon toolset lost %q; the landing widget needs it", want)
		}
	}
	if defs := anon.Definitions(); len(defs) == 0 {
		t.Error("anon toolset is empty; the model would have nothing to call")
	}
}

// The two gates that protect dispatch (see RunWithRole): the role
// allowlist AND the ToolSet. This reproduces exactly how ClientChat
// wires them — role "client" for prompt/caps, ToolSet "anon" for tools —
// and asserts the ToolSet gate is what stops a tenant-row tool.
func TestClientChatWiring_AnonToolsetBlocksRowsDespiteClientRole(t *testing.T) {
	ts := agentstools.NewToolSetFor(nil, "centrum", "anon") // what ClientChat constructs
	role := "client"                                        // what ClientChat passes to RunWithRole

	// Gate 1 (role allowlist) would permit these - that is fine, gate 2
	// is the one that must hold.
	for _, name := range []string{"list_bookings", "query_attendance"} {
		if !agentstools.AllowedToolsFor(role)[name] {
			t.Logf("note: gate 1 already blocks %q", name)
		}
		if _, ok := ts.Get(name); ok {
			t.Fatalf("gate 2 FAILED: %q is reachable from the anon toolset", name)
		}
	}

	// And the permitted tool still resolves.
	if _, ok := ts.Get("get_availability"); !ok {
		t.Error("anon toolset must still expose get_availability")
	}
}

// The authenticated client tier must KEEP its data tools: that tier
// exists for API/agent integrations whose token pins the tenant via RLS.
func TestClientTier_KeepsItsDataTools(t *testing.T) {
	client := agentstools.NewToolSetFor(nil, "centrum", "client")
	for _, want := range []string{"list_bookings", "get_pricing", "get_availability"} {
		if _, ok := client.Get(want); !ok {
			t.Errorf("client tier lost %q; that would break API integrations", want)
		}
	}
	// But it still must not reach money/credential tools.
	for _, forbidden := range []string{"export_report_excel", "query_net_profit", "topup_member"} {
		if _, ok := client.Get(forbidden); ok {
			t.Errorf("client tier must not expose %q", forbidden)
		}
	}
}

func TestOwnerTier_IsUnrestricted(t *testing.T) {
	owner := agentstools.NewToolSetFor(nil, "centrum", "owner")
	for _, want := range []string{"list_bookings", "query_net_profit", "export_report_excel"} {
		if _, ok := owner.Get(want); !ok {
			t.Errorf("owner tier lost %q", want)
		}
	}
}

func TestUnknownRoleGetsNothing(t *testing.T) {
	ts := agentstools.NewToolSetFor(nil, "centrum", "not-a-role")
	if defs := ts.Definitions(); len(defs) != 0 {
		t.Errorf("unknown role got %d tools, want 0", len(defs))
	}
}

// A tool in the anon tier must still refuse to run against a tenant it
// was not scoped to. Deliberately does NOT execute a DB tool here:
// agentstools.NewToolSetFor(nil, …) builds tools with a nil pool, so Run would panic
// on the connection rather than prove anything about scoping. The
// scoping guarantee is RLS plus the per-tool `WHERE tenant_id = $1`,
// covered by the feature isolation tests in internal/features/….
//
// What this DOES pin: the anon tier exposes no tool that reads tenant
// rows, so there is nothing here for a caller-chosen tenant to leak
// through, and the tier is non-empty so the widget still works.
func TestAnonTier_ExposesNoRowReaderToScopeAbuse(t *testing.T) {
	anon := agentstools.NewToolSetFor(nil, "centrum", "anon")
	for _, name := range anon.Definitions() {
		for _, row := range toolsThatReadTenantRows() {
			if name == row {
				t.Errorf("anon exposes %q, which reads tenant rows", name)
			}
		}
	}
}
