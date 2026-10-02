package adkrunner

import (
	"strings"
	"testing"

	"github.com/davinakmalyasha/agents-proxy/AgentsTools"
	_ "github.com/davinakmalyasha/agents-proxy/tenants/centrum-badminton/tools"
	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
)

// TestCentrumProjectValidatesAgainstTheRegistry is the boot check, run in a
// test so a bad tools.json fails CI rather than a deploy.
//
// It compares the project file against the LIVE registry: every tool it
// names must exist, every Mutating flag must match the Go one, and no role
// may exceed the platform ceiling. Those are the three ways a config file
// could describe a system that is not the one running.
func TestCentrumProjectValidatesAgainstTheRegistry(t *testing.T) {
	ts := agentstools.NewToolSet(nil)
	projects, err := agentstools.LoadProjects(ts)
	if err != nil {
		t.Fatalf("projects must load and validate at boot: %v", err)
	}
	p, ok := projects["centrum"]
	if !ok {
		t.Fatalf("no project for the centrum tenant; got %d projects", len(projects))
	}
	if p.Display == "" || p.Vertical != "sports" {
		t.Errorf("project identity wrong: display=%q vertical=%q", p.Display, p.Vertical)
	}
	// Every tool the project declares must be one the registry can actually
	// run -- this is the assertion that would have caught a rename.
	for _, spec := range p.Tools {
		if _, ok := ts.Get(spec.Name); !ok {
			t.Errorf("project declares %q, which the registry does not implement", spec.Name)
		}
	}
}

// TestProjectRoutingSurvivesTheMove is deleted with the router: the model
// chooses tools from Description + schema now, so there is no routing for
// the move to preserve. The ApplyProject narrowing tests below stay --
// they pin the platform ceiling, which is unchanged.

// TestApplyProjectCannotWidenAccess is the security property. The project
// file may remove tools and may remove roles; it may never grant either.
func TestApplyProjectCannotWidenAccess(t *testing.T) {
	base := agentstools.NewToolSetFor(nil, "pegawai")
	before := base.Names()

	// A project that claims a tool the role cannot call, and gives a tool
	// roles beyond the ceiling.
	p := &agentconfig.Project{
		Slug:     "evil",
		Vertical: "sports",
		Tools: []agentconfig.ToolSpec{
			// query_net_profit is owner-only. A config naming it must not
			// put it in a pegawai's hands.
			{Name: "query_net_profit", Roles: []string{"pegawai", "client"}},
			{Name: "list_bookings", Roles: []string{"owner", "pegawai"}},
		},
	}
	applied := base.ApplyProject(p)

	for _, name := range applied.Names() {
		if name == "query_net_profit" {
			t.Error("a project config put an owner-only tool into a pegawai tool set")
		}
	}
	// Narrowing: list_bookings is declared for owner+pegawai, and the
	// pegawai set keeps it -- but the role list on the tool must not gain a
	// role the ceiling never gave it.
	if tool, ok := applied.Get("list_bookings"); ok {
		for _, r := range tool.Roles {
			if r == "client" {
				t.Error("ApplyProject granted a role the platform ceiling did not")
			}
		}
	}
	// Narrowing: a project declares what it uses, so the set can only ever
	// shrink. query_net_profit was dropped because the role never had it,
	// and list_bookings survived because it did.
	if len(applied.Names()) > len(before) {
		t.Errorf("ApplyProject grew a role's tool set: %d -> %d", len(before), len(applied.Names()))
	}
	if _, ok := applied.Get("list_bookings"); !ok {
		t.Error("list_bookings was in the role's set and declared by the project, but did not survive")
	}
}

// TestApplyProjectCannotReclassifyAWrite: the Mutating flag comes from Go,
// never from the config. If a project could call book_slot a read, the gate
// would honour it unpinned and the original security hole returns.
func TestApplyProjectCannotReclassifyAWrite(t *testing.T) {
	base := agentstools.NewToolSet(nil)
	no := false
	p := &agentconfig.Project{
		Slug:     "liar",
		Vertical: "sports",
		Tools:    []agentconfig.ToolSpec{{Name: "book_slot", Mutating: &no}},
	}
	applied := base.ApplyProject(p)
	tool, ok := applied.Get("book_slot")
	if !ok {
		t.Fatal("book_slot missing")
	}
	if !tool.Mutating {
		t.Error("a project config reclassified a write tool as a read; the gate trusts the Go flag, so this would reopen the hole")
	}
}

// TestPromptLayersUseProjectContent: the prompt must carry the project's own
// text, and must NOT still be carrying the hardcoded Centrum contact from
// the old Go source.
func TestPromptLayersUseProjectContent(t *testing.T) {
	ts := agentstools.NewToolSet(nil)
	projects, err := agentstools.LoadProjects(ts)
	if err != nil {
		t.Fatal(err)
	}
	p := projects["centrum"]

	got := Assemble(Layers{Project: p, Role: "owner", Vertical: "sports", ToolDefs: ts.Definitions()})
	for _, want := range []string{
		"Centrum Badminton Arena", // from project.json
		"project instructions",
		"reference: venue",
		"reference: money-and-pricing",
		"Today's date is", // runtime layer
		"628159408136",    // the number moved from Go into the project file
	} {
		if !strings.Contains(got, want) {
			t.Errorf("assembled prompt is missing %q", want)
		}
	}
}

// TestSkillsAreAlwaysInjectedWhole: every skill in tenants/<slug>/skills/ is
// injected whole, every turn. A tenant's skills file is the procedures it
// wants followed; injecting a procedure only sometimes means it is followed
// only sometimes.
func TestSkillsAreAlwaysInjectedWhole(t *testing.T) {
	ts := agentstools.NewToolSet(nil)
	projects, err := agentstools.LoadProjects(ts)
	if err != nil {
		t.Fatal(err)
	}
	p := projects["centrum"]

	for _, msg := range []string{
		"tolong bantu tutup buku hari ini",
		"berapa pendapatan bulan ini?",
		"halo",
	} {
		got := Assemble(Layers{Project: p, Role: "owner", Vertical: "sports",
			Message: msg, ToolDefs: ts.Definitions()})
		if !strings.Contains(got, "procedure: Badminton Skills") {
			t.Errorf("message %q: the skills file was not injected whole", msg)
		}
	}
}

// TestPromptWithoutProjectStillWorks: an unconfigured tenant must keep a
// working agent. Config is an addition, not a precondition.
func TestPromptWithoutProjectStillWorks(t *testing.T) {
	ts := agentstools.NewToolSet(nil)
	got := Assemble(Layers{Role: "owner", Vertical: "sports", ToolDefs: ts.Definitions()})
	if len(got) < 200 {
		t.Errorf("a tenant with no project config got a near-empty prompt (%d chars)", len(got))
	}
	for _, want := range []string{"never answer from memory", "OWNER", "Tools you can call right now"} {
		if !strings.Contains(got, want) {
			t.Errorf("base prompt missing %q", want)
		}
	}
}
