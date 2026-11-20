package agentconfig

import (
	"strings"
	"testing"
)

// TestProjectsLoadFromTheProjectsTree is the one that would have caught a
// missing or malformed project folder before it reached a running agent.
// Parsing happens at boot, so this is cheap insurance that the embedded
// tree is actually loadable.
func TestProjectsLoadFromTheProjectsTree(t *testing.T) {
	projects, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(projects) == 0 {
		t.Fatal("no projects embedded")
	}
	p, ok := projects["centrum"]
	if !ok {
		t.Fatalf("the centrum project is not embedded; got %v", keys(projects))
	}
	// The directory may be written however it reads best; the slug is the
	// tenant id and must be a clean identifier, because it is what every
	// WHERE tenant_id and RLS policy keys on.
	if p.Display != "Centrum Badminton Arena" {
		t.Errorf("display = %q", p.Display)
	}
	if p.Vertical != "sports" {
		t.Errorf("vertical = %q, want sports", p.Vertical)
	}
	if p.Schema != "project_centrum" {
		t.Errorf("schema = %q, want project_centrum", p.Schema)
	}
	if !p.Agent.Enabled {
		t.Error("centrum agent should be enabled")
	}
	if strings.TrimSpace(p.SystemPrompt) == "" {
		t.Error("SystemPrompt.md is empty")
	}
	if len(p.Skills) == 0 {
		t.Error("no skills loaded")
	}
	// The persona must not carry the old template placeholders: the
	// assembler appends live date/tool context itself, and literal
	// {{DATE}} / {{TOOLS}} would reach the model as noise.
	for _, ph := range []string{"{{DATE}}", "{{TOOLS}}"} {
		if strings.Contains(p.SystemPrompt, ph) {
			t.Errorf("agent.md still contains the %s placeholder", ph)
		}
	}
}

// TestSkillBodiesLoad: every .md under projects/<slug>/agent/skills/ is one skill,
// injected whole. An empty file is a procedure that says nothing.
func TestSkillBodiesLoad(t *testing.T) {
	projects, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p := projects["centrum"]
	if len(p.Skills) != 1 {
		t.Fatalf("expected the single BadmintonSkills.md, got %d skills", len(p.Skills))
	}
	s := p.Skills[0]
	if strings.TrimSpace(s.Body) == "" {
		t.Errorf("skill %q loaded with an empty body", s.Name)
	}
	if s.Title == "" {
		t.Errorf("skill %q has no title", s.Name)
	}
	if s.Title != "Badminton Skills — Centrum Badminton Arena" {
		t.Errorf("skill title = %q, want the first H1 heading", s.Title)
	}
}

// TestAllDiscoveredProjectsValidate: with tools and backend compiled in,
// the only thing that can make a tenant half-present is a malformed
// declarative folder -- and that must fail here, not in production boot.
// (The old cross-folder agreement check died with the four folders:
// there is only one project tree now, discovered from projects/.)
func TestAllDiscoveredProjectsValidate(t *testing.T) {
	projects, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for slug, p := range projects {
		if slug == "" {
			t.Error("a tenant loaded with an empty slug")
		}
		// Same shape as the centrum check below: identity, budgets,
		// skills and knowledge, no registry needed.
		if err := Validate(p, DefaultBudget()); err != nil {
			t.Errorf("tenant %q invalid: %v", slug, err)
		}
	}
}

// TestValidateCatchesConfigDrift is the boot-time safety net. Each case is a
// way a tenant folder can be wrong in a way that would otherwise produce a
// silently broken agent.
func TestValidateCatchesConfigDrift(t *testing.T) {
	b := DefaultBudget()

	good := &Project{
		Slug:     "demo",
		Vertical: "sports",
		Schema:   "project_demo",
	}
	if err := Validate(good, b); err != nil {
		t.Fatalf("a valid project was rejected: %v", err)
	}

	for name, mutate := range map[string]func(*Project){
		"a skill with no body": func(p *Project) {
			p.Skills = []Skill{{Name: "empty"}}
		},
		"an oversized system prompt": func(p *Project) {
			p.SystemPrompt = strings.Repeat("x", b.MaxSystemPromptRunes+1)
		},
		"oversized information": func(p *Project) {
			p.Information = []Document{{Name: "big", Body: strings.Repeat("y", b.MaxInformationRunes+1)}}
		},
		"no vertical":        func(p *Project) { p.Vertical = "" },
		"a slug with spaces": func(p *Project) { p.Slug = "My Venue" },
	} {
		p := &Project{Slug: "demo", Vertical: "sports", Schema: "project_demo"}
		mutate(p)
		if err := Validate(p, b); err == nil {
			t.Errorf("%s: expected validation to fail, got nil", name)
		}
	}
}

// TestCentrumConfigIsValid runs the real embedded project through Validate,
// so a malformed folder fails here rather than in production boot.
func TestCentrumConfigIsValid(t *testing.T) {
	projects, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p := projects["centrum"]
	// Tool implementations live in projects/<slug>/agent/tools/*.go now, not in the
	// folders, so there is no per-tool registry to check here. What this
	// asserts is everything the folders DO own: identity, budgets, skills
	// and knowledge.
	if err := Validate(p, DefaultBudget()); err != nil {
		t.Fatalf("centrum config invalid: %v", err)
	}
}

func keys(m map[string]*Project) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
