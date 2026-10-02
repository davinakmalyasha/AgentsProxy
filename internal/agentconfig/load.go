package agentconfig

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// projectsFS carries every project's agents/ directory into the binary.
//
// Embed rather than read from disk at runtime. A per-tenant folder that
// only exists on one machine is not a tenant configuration, it is a
// deployment accident -- and reading it would also mean path traversal is
// now a live input (ARCHITECTURE.md records the CVE that os.Root exists to
// prevent, and embedding removes the class entirely).
//
//go:embed all:projects
var projectsFS embed.FS

// projectsRoot is where the embedded tree starts.
const projectsRoot = "projects"

// Load parses every project under projects/*/agents/.
//
// It does not validate against a tool registry; call Validate with the live
// registry once the tools are built. Separation matters: config can be
// parsed without a database, so a config error is reported as a config
// error rather than surfacing as a connection failure.
func Load() (map[string]*Project, error) {
	entries, err := fs.ReadDir(projectsFS, projectsRoot)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", projectsRoot, err)
	}
	out := make(map[string]*Project, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "_") {
			continue // _template is a starting point, not a live project
		}
		p, err := loadOne(e.Name())
		if err != nil {
			return nil, err
		}
		out[p.Slug] = p
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no projects found under %s", projectsRoot)
	}
	return out, nil
}

func loadOne(dir string) (*Project, error) {
	root := path.Join(projectsRoot, dir, "agents")
	// The directory name is a label, not an identifier. The slug in
	// project.json is the tenant id and is what every WHERE tenant_id and
	// RLS policy keys on, so it is validated as one; falling back to the
	// lowercased directory name keeps a project working if project.json is
	// forgotten, which is a mistake worth having a visible default for.
	p := &Project{Slug: strings.ToLower(dir), Display: dir}

	if raw, err := fs.ReadFile(projectsFS, path.Join(root, "project.json")); err == nil {
		var doc struct {
			Slug     string `json:"slug"`
			Display  string `json:"display"`
			Vertical string `json:"vertical"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("project %q: project.json is not valid JSON: %w", dir, err)
		}
		if doc.Slug != "" {
			p.Slug = doc.Slug
		}
		if doc.Display != "" {
			p.Display = doc.Display
		}
		p.Vertical = doc.Vertical
	} else if !isNotExist(err) {
		return nil, fmt.Errorf("project %q: read project.json: %w", dir, err)
	}

	// tools.json is optional: a project may run entirely on the platform
	// defaults. Absent is fine; malformed is not.
	raw, err := fs.ReadFile(projectsFS, path.Join(root, "tools.json"))
	if err == nil {
		// Object form so the file can carry a comment explaining what it is
		// allowed to do -- particularly that roles can only narrow.
		var doc struct {
			Tools []ToolSpec `json:"tools"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("project %q: tools.json is not valid JSON: %w", dir, err)
		}
		p.Tools = doc.Tools
	} else if !isNotExist(err) {
		return nil, fmt.Errorf("project %q: read tools.json: %w", dir, err)
	}

	if b, err := fs.ReadFile(projectsFS, path.Join(root, "SystemPrompt.md")); err == nil {
		p.SystemPrompt = string(b)
	} else if !isNotExist(err) {
		return nil, fmt.Errorf("project %q: read SystemPrompt.md: %w", dir, err)
	}

	if p.Information, err = loadDocs(root, "Information"); err != nil {
		return nil, fmt.Errorf("project %q: %w", dir, err)
	}
	if p.Skills, err = loadSkills(root); err != nil {
		return nil, fmt.Errorf("project %q: %w", dir, err)
	}

	if raw, err := fs.ReadFile(projectsFS, path.Join(root, "mcp.json")); err == nil {
		// Object form, not a bare array, so the file can carry a comment
		// explaining why the list is empty -- which is the first question
		// anyone asks when they open it.
		var doc struct {
			Servers []MCPServer `json:"servers"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("project %q: mcp.json is not valid JSON: %w", dir, err)
		}
		p.MCP = doc.Servers
	} else if !isNotExist(err) {
		return nil, fmt.Errorf("project %q: read mcp.json: %w", dir, err)
	}
	return p, nil
}

// loadDocs reads every .md under agents/<dir>/ as one knowledge document
// each, in sorted order so the assembled prompt is deterministic.
func loadDocs(root, dir string) ([]Document, error) {
	entries, err := fs.ReadDir(projectsFS, path.Join(root, dir))
	if err != nil {
		if isNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s/: %w", dir, err)
	}
	var out []Document
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		b, err := fs.ReadFile(projectsFS, path.Join(root, dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s/%s: %w", dir, e.Name(), err)
		}
		out = append(out, Document{
			Name: strings.TrimSuffix(e.Name(), ".md"),
			Body: strings.TrimSpace(string(b)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// skillsMeta is the sidecar describing each skill file: its title and the
// words that make it relevant. A skill with no triggers would be injected
// into every conversation, so the triggers live beside the body rather than
// being guessed out of the prose.
type skillsMeta struct {
	Skills []struct {
		Name     string   `json:"name"`
		Title    string   `json:"title"`
		Triggers []string `json:"triggers"`
		Keywords []string `json:"keywords"`
	} `json:"skills"`
}

func loadSkills(root string) ([]Skill, error) {
	raw, err := fs.ReadFile(projectsFS, path.Join(root, "skills", "index.json"))
	if err != nil {
		if isNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read skills/index.json: %w", err)
	}
	var meta skillsMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("skills/index.json is not valid JSON: %w", err)
	}
	out := make([]Skill, 0, len(meta.Skills))
	for _, m := range meta.Skills {
		body, err := fs.ReadFile(projectsFS, path.Join(root, "skills", m.Name+".md"))
		if err != nil {
			// The index naming a file that is not there is a config bug,
			// not an absence: it would silently drop the procedure.
			return nil, fmt.Errorf("skills/index.json names %q but skills/%s.md is missing", m.Name, m.Name)
		}
		out = append(out, Skill{
			Name:     m.Name,
			Title:    m.Title,
			Triggers: m.Triggers,
			Keywords: m.Keywords,
			Body:     strings.TrimSpace(string(body)),
		})
	}
	return out, nil
}

func isNotExist(err error) bool {
	return err != nil && strings.Contains(err.Error(), "file does not exist")
}
