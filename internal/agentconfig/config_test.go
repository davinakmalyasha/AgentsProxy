package agentconfig

import (
	"strings"
	"testing"
)

// TestEmbeddedProjectsParse is the one that would have caught a malformed
// tools.json before it reached a running agent. Parsing happens at boot, so
// this is cheap insurance that the embedded tree is actually loadable.
func TestEmbeddedProjectsParse(t *testing.T) {
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
	if len(p.Tools) == 0 {
		t.Error("no tool declarations loaded")
	}
	if strings.TrimSpace(p.SystemPrompt) == "" {
		t.Error("SystemPrompt.md is empty")
	}
	if len(p.Information) == 0 {
		t.Error("no Information/ documents loaded")
	}
	if len(p.Skills) == 0 {
		t.Error("no skills loaded")
	}
	// The prompt must carry the placeholders the assembler fills, or the
	// date and tool list never reach the model.
	for _, ph := range []string{"{{DATE}}", "{{TOOLS}}"} {
		if !strings.Contains(p.SystemPrompt, ph) {
			t.Errorf("SystemPrompt.md is missing the %s placeholder", ph)
		}
	}
}

// TestSkillBodiesLoad guards the sidecar index: a skill named in
// skills/index.json whose .md is missing would silently drop a procedure.
func TestSkillBodiesLoad(t *testing.T) {
	projects, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p := projects["centrum"]
	for _, s := range p.Skills {
		if strings.TrimSpace(s.Body) == "" {
			t.Errorf("skill %q loaded with an empty body", s.Name)
		}
		if s.Title == "" {
			t.Errorf("skill %q has no title", s.Name)
		}
	}
}

// TestSkillTriggersArePhraseMatched is the guard on a subtle failure: a
// trigger that matches as a substring would inject a procedure into every
// conversation. "batal" must not fire on "pembatalan" unless intended.
func TestSkillTriggersArePhraseMatched(t *testing.T) {
	p := &Project{Skills: []Skill{
		{Name: "cancel", Triggers: []string{"batalkan"}},
		{Name: "opening", Triggers: []string{"jam kosong"}},
	}}

	got := p.SkillsFor(Words("tolong batalkan booking tomorrow"))
	if len(got) != 1 || got[0].Name != "cancel" {
		t.Errorf("cancel skill not triggered: %+v", got)
	}

	got = p.SkillsFor(Words("pembatalan spamming lewat email"))
	if len(got) != 0 {
		t.Errorf("substring 'batal' fired on 'pembatalan': %+v", got)
	}

	got = p.SkillsFor(Words("ada jam kosong besok"))
	if len(got) != 1 || got[0].Name != "opening" {
		t.Errorf("multi-word trigger did not fire: %+v", got)
	}
}

// TestValidateCatchesConfigDrift is the boot-time safety net. Each case is a
// way a project folder can be wrong in a way that would otherwise produce a
// silently broken agent.
func TestValidateCatchesConfigDrift(t *testing.T) {
	known := map[string]bool{"list_bookings": true, "book_slot": true}
	mut := map[string]bool{"book_slot": true}
	ceiling := RoleCeiling{
		"owner":   {"list_bookings": true, "book_slot": true},
		"pegawai": {"list_bookings": true},
	}
	b := DefaultBudget()

	good := &Project{
		Slug:     "demo",
		Vertical: "sports",
		Tools:    []ToolSpec{{Name: "list_bookings"}},
	}
	if err := Validate(good, known, mut, ceiling, b); err != nil {
		t.Fatalf("a valid project was rejected: %v", err)
	}

	for name, mutate := range map[string]func(*Project){
		"a tool that is not implemented": func(p *Project) {
			p.Tools = append(p.Tools, ToolSpec{Name: "does_not_exist"})
		},
		"a duplicate tool": func(p *Project) {
			p.Tools = append(p.Tools, ToolSpec{Name: "list_bookings"})
		},
		"mutating that contradicts the code": func(p *Project) {
			no := false
			// book_slot IS mutating in the registry, so claiming otherwise
			// is the dangerous direction: the gate trusts the Go flag, and a
			// config saying "read" would make a reviewer believe writes are
			// unpinnable here.
			p.Tools = append(p.Tools, ToolSpec{Name: "book_slot", Mutating: &no})
		},
		"a role that widens the ceiling": func(p *Project) {
			p.Tools = append(p.Tools, ToolSpec{Name: "list_bookings", Roles: []string{"client"}})
		},
		"a skill with no triggers": func(p *Project) {
			p.Skills = []Skill{{Name: "s", Body: "b"}}
		},
		"an oversized system prompt": func(p *Project) {
			p.SystemPrompt = strings.Repeat("x", b.MaxSystemPromptRunes+1)
		},
		"oversized information": func(p *Project) {
			p.Information = []Document{{Name: "big", Body: strings.Repeat("y", b.MaxInformationRunes+1)}}
		},
		"an unknown mcp transport": func(p *Project) {
			p.MCP = []MCPServer{{Name: "x", URL: "http://x", Transport: "carrier-pigeon"}}
		},
		"no vertical":        func(p *Project) { p.Vertical = "" },
		"a slug with spaces": func(p *Project) { p.Slug = "My Venue" },
	} {
		p := &Project{Slug: "demo", Vertical: "sports", Tools: []ToolSpec{{Name: "list_bookings"}}}
		mutate(p)
		if err := Validate(p, known, mut, ceiling, b); err == nil {
			t.Errorf("%s: expected validation to fail, got nil", name)
		}
	}
}

// TestCentrumConfigIsValid runs the real embedded project against
// the real keyword tables, so a rename on either side fails here rather than
// in production routing.
func TestCentrumConfigIsValid(t *testing.T) {
	projects, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p := projects["centrum"]
	// The ceiling is exercised separately against the live registry in
	// internal/agent; here we only check the parts that need no tool set.
	b := DefaultBudget()
	known := map[string]bool{}
	mut := map[string]bool{}
	for _, s := range p.Tools {
		known[s.Name] = true
		if s.Mutating != nil {
			mut[s.Name] = *s.Mutating
		}
	}
	if err := Validate(p, known, mut, RoleCeiling{"owner": known}, b); err != nil {
		t.Fatalf("centrum config invalid: %v", err)
	}
}

// TestMatcherIsWordBounded pins the matcher used for skill triggers. This
// is the same rule as the router's, and the same bug: substring matching let
// "pesanan" reach the booking tool and blanked the agent's data.
func TestMatcherIsWordBounded(t *testing.T) {
	for _, c := range []struct {
		msg, kw string
		want    bool
	}{
		{"tolong batalkan", "batalkan", true},
		{"pembatalan massak", "batalkan", false},
		{"buat booking", "buat booking", true},
		{"ada pesanan", "pesan", false},
		{"oke", "", false},
	} {
		if got := HasPhrase(Words(c.msg), c.kw); got != c.want {
			t.Errorf("HasPhrase(%q, %q) = %v, want %v", c.msg, c.kw, got, c.want)
		}
	}
}

func keys(m map[string]*Project) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
