package agentconfig

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	agentprojects "github.com/davinakmalyasha/agents-proxy"
)

// Load parses every tenant under tenants/<slug>/.
//
// Tenants are discovered from tenants/: every subdirectory except
// _template is a tenant. Tools and backend are compiled Go, not folders
// the loader reads -- a tenant's code reaches the binary through blank
// imports in main.go, while this loader reads only the declarative
// parts (memory, skills, mcp). The two halves meet at boot validation,
// which checks the loaded config against the live tool registry.
//
// It does not validate against a tool registry; call Validate with the live
// registry once the tools are built. Separation matters: config can be
// parsed without a database, so a config error is reported as a config
// error rather than surfacing as a connection failure.
func Load() (map[string]*Project, error) {
	tenantDirs, err := listTenants("tenants")
	if err != nil {
		return nil, err
	}

	out := make(map[string]*Project, len(tenantDirs))
	for dir := range tenantDirs {
		p, err := loadOne(dir)
		if err != nil {
			return nil, err
		}
		out[p.Slug] = p
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no tenants found under tenants/")
	}
	return out, nil
}

// listTenants returns the non-template directories under a folder of the
// embedded tree (tenants/ at top level, _template skipped).
func listTenants(folder string) (map[string]bool, error) {
	entries, err := fs.ReadDir(agentprojects.AgentsFS, folder)
	if err != nil {
		return nil, fmt.Errorf("read %s/: %w", folder, err)
	}
	out := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "_") {
			continue // _template is a starting point, not a live tenant
		}
		out[e.Name()] = true
	}
	return out, nil
}

func loadOne(dir string) (*Project, error) {
	// The directory name is a label, not an identifier. The slug in
	// project.json is the tenant id and is what every WHERE tenant_id and
	// RLS policy keys on, so it is validated as one; falling back to the
	// lowercased directory name keeps a project working if project.json is
	// forgotten, which is a mistake worth having a visible default for.
	p := &Project{Slug: strings.ToLower(dir), Display: dir}
	memoryRoot := path.Join("tenants", dir, "memory")

	if raw, err := fs.ReadFile(agentprojects.AgentsFS, path.Join(memoryRoot, "project.json")); err == nil {
		var doc struct {
			Slug     string `json:"slug"`
			Display  string `json:"display"`
			Vertical string `json:"vertical"`
			Contact  string `json:"contact"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("tenant %q: project.json is not valid JSON: %w", dir, err)
		}
		if doc.Slug != "" {
			p.Slug = doc.Slug
		}
		if doc.Display != "" {
			p.Display = doc.Display
		}
		p.Vertical = doc.Vertical
		p.Contact = doc.Contact
	} else if !isNotExist(err) {
		return nil, fmt.Errorf("tenant %q: read project.json: %w", dir, err)
	}

	if b, err := fs.ReadFile(agentprojects.AgentsFS, path.Join(memoryRoot, "SystemPrompt.md")); err == nil {
		p.SystemPrompt = string(b)
	} else if !isNotExist(err) {
		return nil, fmt.Errorf("tenant %q: read SystemPrompt.md: %w", dir, err)
	}

	var infoDocs []Document
	var skills []Skill
	var err error
	if infoDocs, err = loadDocs(path.Join(memoryRoot, "Information")); err != nil {
		return nil, fmt.Errorf("tenant %q: %w", dir, err)
	}
	p.Information = infoDocs
	if skills, err = loadSkills(dir); err != nil {
		return nil, fmt.Errorf("tenant %q: %w", dir, err)
	}
	p.Skills = skills

	if raw, err := fs.ReadFile(agentprojects.AgentsFS, path.Join("tenants", dir, "mcp", "mcp.json")); err == nil {
		// Object form, not a bare array, so the file can carry a comment
		// explaining why the list is empty -- which is the first question
		// anyone asks when they open it.
		var doc struct {
			Servers []MCPServer `json:"servers"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("tenant %q: mcp.json is not valid JSON: %w", dir, err)
		}
		p.MCP = doc.Servers
	} else if !isNotExist(err) {
		return nil, fmt.Errorf("tenant %q: read mcp.json: %w", dir, err)
	}
	return p, nil
}

// loadDocs reads every .md under dir as one knowledge document each, in
// sorted order so the assembled prompt is deterministic.
func loadDocs(dir string) ([]Document, error) {
	entries, err := fs.ReadDir(agentprojects.AgentsFS, dir)
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
		b, err := fs.ReadFile(agentprojects.AgentsFS, path.Join(dir, e.Name()))
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

// loadSkills reads every .md under tenants/<dir>/skills/ as one skill each.
//
// Every skill is always injected, whole. There is no trigger index and no
// selective matching: a tenant's skills file is the procedures it wants
// followed, and a procedure injected only sometimes is a procedure the model
// follows only sometimes. If the file ever grows noisy enough that always-on
// hurts, split it -- do not add a matching layer on top.
func loadSkills(dir string) ([]Skill, error) {
	entries, err := fs.ReadDir(agentprojects.AgentsFS, path.Join("tenants", dir, "skills"))
	if err != nil {
		if isNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read tenants/%s/skills/: %w", dir, err)
	}
	var out []Skill
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		b, err := fs.ReadFile(agentprojects.AgentsFS, path.Join("tenants", dir, "skills", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read tenants/%s/skills/%s: %w", dir, e.Name(), err)
		}
		out = append(out, Skill{
			Name:  strings.TrimSuffix(e.Name(), ".md"),
			Title: skillTitle(string(b), strings.TrimSuffix(e.Name(), ".md")),
			Body:  strings.TrimSpace(string(b)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// skillTitle takes the first "# " heading as the title, falling back to the
// file name. The title is what the assembled prompt calls the procedure.
func skillTitle(body, fallback string) string {
	for _, line := range strings.Split(body, "\n") {
		if t, ok := strings.CutPrefix(strings.TrimSpace(line), "# "); ok && t != "" {
			return t
		}
	}
	return fallback
}

func isNotExist(err error) bool {
	return err != nil && strings.Contains(err.Error(), "file does not exist")
}
