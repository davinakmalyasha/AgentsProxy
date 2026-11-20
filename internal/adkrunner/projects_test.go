package adkrunner

import (
	"strings"
	"testing"

	"github.com/davinakmalyasha/agents-proxy/AgentsTools"
	_ "github.com/davinakmalyasha/agents-proxy/projects/centrum/agent/tools"
)

// TestCentrumProjectValidatesAgainstTheRegistry is the boot check, run in a
// test so a malformed tenant folder fails CI rather than a deploy.
//
// Tool implementations live in projects/<slug>/agent/tools/*.go next to their
// implementations, so there is no per-tool registry to check here. What this
// asserts is the boot path itself: projects load, validate, and carry the
// tenant's identity.
func TestCentrumProjectValidatesAgainstTheRegistry(t *testing.T) {
	projects, err := agentstools.LoadProjects(agentstools.AllTools(nil))
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
}

// TestProjectRoutingSurvivesTheMove is deleted with the router: the model
// chooses tools from Description + schema now, so there is no routing for
// the move to preserve. Role tool sets come from NewToolSetFor intersected
// with the platform ceiling in AllowedToolsFor, which isolation_test.go pins.

// TestPromptLayersUseProjectContent: the prompt must carry the project's own
// text, and must NOT still be carrying the hardcoded Centrum contact from
// the old Go source.
func TestPromptLayersUseProjectContent(t *testing.T) {
	ts := agentstools.NewToolSetFor(nil, "centrum", "owner")
	projects, err := agentstools.LoadProjects(agentstools.AllTools(nil))
	if err != nil {
		t.Fatal(err)
	}
	p := projects["centrum"]

	got := Assemble(Layers{Project: p, Role: "owner", Vertical: "sports", ToolDefs: ts.Definitions()})
	for _, want := range []string{
		"Centrum Badminton Arena", // from project.json
		"project instructions",
		"Opening hours and courts", // from agent/agent.md
		"Money, pricing and how the numbers are counted",
		"Today's date is", // runtime layer
		"628159408136",    // the number lives in agent/agent.md
	} {
		if !strings.Contains(got, want) {
			t.Errorf("assembled prompt is missing %q", want)
		}
	}
}

// TestSkillsAreAlwaysInjectedWhole: every skill in projects/<slug>/agent/skills/ is
// injected whole, every turn. A tenant's skills file is the procedures it
// wants followed; injecting a procedure only sometimes means it is followed
// only sometimes.
func TestSkillsAreAlwaysInjectedWhole(t *testing.T) {
	ts := agentstools.NewToolSetFor(nil, "centrum", "owner")
	projects, err := agentstools.LoadProjects(agentstools.AllTools(nil))
	if err != nil {
		t.Fatal(err)
	}
	p := projects["centrum"]

	got := Assemble(Layers{Project: p, Role: "owner", Vertical: "sports", ToolDefs: ts.Definitions()})
	if !strings.Contains(got, "procedure: Badminton Skills") {
		t.Error("the skills file was not injected whole")
	}
}

// TestPromptWithoutProjectStillWorks: an unconfigured tenant must keep a
// working agent. Config is an addition, not a precondition.
func TestPromptWithoutProjectStillWorks(t *testing.T) {
	ts := agentstools.NewToolSetFor(nil, "centrum", "owner")
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
