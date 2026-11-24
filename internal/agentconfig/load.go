package agentconfig

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	projectembed "github.com/davinakmalyasha/agents-proxy"
)

// Load parses every project under projects/<slug>/.
//
// Projects are discovered from projects/: every visible subdirectory is a
// project. Backend and agent tools are compiled Go, not folders the loader
// reads -- a project's code reaches the binary through the generated blank
// imports, while this loader reads only the declarative parts (project.json,
// agent.md, skills, MCP config).
//
// It does not validate against a tool registry; call Validate with the live
// registry once the tools are built. Separation matters: config can be
// parsed without a database, so a config error is reported as a config
// error rather than surfacing as a connection failure.
func Load() (map[string]*Project, error) {
	good, bad := LoadCollect()
	if len(bad) > 0 {
		msgs := make([]string, 0, len(bad))
		for dir, err := range bad {
			msgs = append(msgs, dir+": "+err.Error())
		}
		sort.Strings(msgs)
		return nil, fmt.Errorf("bad project folders: %s", strings.Join(msgs, "; "))
	}
	if len(good) == 0 {
		return nil, fmt.Errorf("no projects found under projects/")
	}
	return good, nil
}

// LoadCollect parses every project, collecting per-project failures instead
// of aborting on the first bad folder. Quarantine boot uses this: bad
// projects are fenced individually while healthy ones serve.
func LoadCollect() (good map[string]*Project, bad map[string]error) {
	dirs, err := listProjects("projects")
	if err != nil {
		return nil, map[string]error{"": err}
	}

	good = make(map[string]*Project, len(dirs))
	bad = make(map[string]error)
	seenDir := make(map[string]string, len(dirs))
	for dir := range dirs {
		p, err := loadOne(dir)
		if err != nil {
			// Key by the slug the project claims (what requests use),
			// falling back to the directory when even that is unreadable.
			bad[slugOf(dir)] = err
			continue
		}
		if priorDir, exists := seenDir[p.Slug]; exists {
			bad[p.Slug] = fmt.Errorf("project slug %q is declared by both projects/%s and projects/%s", p.Slug, priorDir, dir)
			continue
		}
		if err := Validate(p, DefaultBudget()); err != nil {
			bad[p.Slug] = err
			continue
		}
		seenDir[p.Slug] = dir
		good[p.Slug] = p
	}
	return good, bad
}

// slugOf is the best-effort slug for a directory whose full load failed:
// the project.json claim if parseable, else the directory name.
func slugOf(dir string) string {
	raw, err := fs.ReadFile(projectembed.FS, path.Join("projects", dir, "project.json"))
	if err != nil {
		return strings.ToLower(dir)
	}
	var doc struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Slug == "" {
		return strings.ToLower(dir)
	}
	return doc.Slug
}

// LoadSingle re-parses one project directory (for the admin reload path).
func LoadSingle(dir string) (*Project, error) {
	return loadOne(dir)
}

// DirForSlug resolves a project slug to its directory: the directory whose
// project.json claims the slug, or the slug itself when it names a directory
// directly.
func DirForSlug(slug string) (string, error) {
	dirs, err := listProjects("projects")
	if err != nil {
		return "", err
	}
	if dirs[slug] {
		return slug, nil
	}
	for dir := range dirs {
		if slugOf(dir) == slug {
			return dir, nil
		}
	}
	return "", fmt.Errorf("unknown project %q", slug)
}

// listProjects returns the live directories under a folder of the embedded
// tree. Directories beginning with "_" are intentionally ignored.
func listProjects(folder string) (map[string]bool, error) {
	entries, err := fs.ReadDir(projectembed.FS, folder)
	if err != nil {
		return nil, fmt.Errorf("read %s/: %w", folder, err)
	}
	out := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		out[e.Name()] = true
	}
	return out, nil
}

type projectDoc struct {
	Slug     string   `json:"slug"`
	Display  string   `json:"display"`
	Vertical string   `json:"vertical"`
	Contact  string   `json:"contact"`
	Schema   string   `json:"schema"`
	Features []string `json:"features"`
	// StrictFeatures turns the feature list into a minimum entitlement.
	StrictFeatures bool `json:"strict_features"`
	Agent          struct {
		Enabled bool     `json:"enabled"`
		Model   string   `json:"model"`
		Persona string   `json:"persona"`
		Skills  string   `json:"skills"`
		MCP     string   `json:"mcp"`
		Tools   []string `json:"tools"`
	} `json:"agent"`
}

func loadOne(dir string) (*Project, error) {
	p := &Project{Slug: strings.ToLower(dir), Display: dir}

	doc, err := readProjectDoc(dir)
	if err != nil {
		return nil, err
	}
	if doc.Slug != "" {
		if doc.Slug != strings.ToLower(dir) {
			return nil, fmt.Errorf("project folder %q declares slug %q; the folder name and slug must match", dir, doc.Slug)
		}
		p.Slug = doc.Slug
	}
	if doc.Display != "" {
		p.Display = doc.Display
	}
	p.Vertical = doc.Vertical
	p.Contact = doc.Contact
	p.Schema = doc.Schema
	p.Features = doc.Features
	p.StrictFeatures = doc.StrictFeatures
	p.Migrations = countProjectMigrations(dir)
	if p.Schema == "" {
		// A new project is isolated by default. Centrum states
		// "project_centrum" explicitly in its project.json.
		p.Schema = SchemaForSlug(p.Slug)
	}
	p.Agent.Enabled = doc.Agent.Enabled
	p.Agent.Model = doc.Agent.Model
	p.Agent.Tools = doc.Agent.Tools
	p.Agent.MCPFile = doc.Agent.MCP

	personaPath := doc.Agent.Persona
	if personaPath == "" {
		personaPath = "agent/agent.md"
	}
	if b, err := readProjectFile(dir, personaPath); err == nil {
		p.SystemPrompt = string(b)
	} else if !isNotExist(err) {
		return nil, fmt.Errorf("project %q: read %s: %w", dir, personaPath, err)
	}

	skillsDir := doc.Agent.Skills
	if skillsDir == "" {
		skillsDir = "agent/skills"
	}
	skills, err := loadSkills(dir, skillsDir)
	if err != nil {
		return nil, fmt.Errorf("project %q: %w", dir, err)
	}
	p.Skills = skills

	if p.Agent.MCPFile != "" {
		raw, err := readProjectFile(dir, p.Agent.MCPFile)
		if err != nil {
			return nil, fmt.Errorf("project %q: read %s: %w", dir, p.Agent.MCPFile, err)
		}
		var mcpDoc struct {
			Servers []MCPServer `json:"servers"`
		}
		if err := json.Unmarshal(raw, &mcpDoc); err != nil {
			return nil, fmt.Errorf("project %q: %s is not valid JSON: %w", dir, p.Agent.MCPFile, err)
		}
		p.Agent.MCP = mcpDoc.Servers
	}

	return p, nil
}

func readProjectDoc(dir string) (*projectDoc, error) {
	raw, err := fs.ReadFile(projectembed.FS, path.Join("projects", dir, "project.json"))
	if err != nil {
		if isNotExist(err) {
			return nil, fmt.Errorf("project %q: project.json is required", dir)
		}
		return nil, fmt.Errorf("project %q: read project.json: %w", dir, err)
	}
	var doc projectDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("project %q: project.json is not valid JSON: %w", dir, err)
	}
	return &doc, nil
}

// readProjectFile reads a project-relative path and rejects path traversal.
// The path comes from project.json (build-time, trusted), but a typo should
// fail at boot, not read a sibling project.
func readProjectFile(dir, rel string) ([]byte, error) {
	clean := path.Clean(strings.TrimPrefix(rel, "/"))
	if clean == "." || strings.HasPrefix(clean, "..") || strings.Contains(clean, "/../") {
		return nil, fmt.Errorf("invalid project-relative path %q", rel)
	}
	return fs.ReadFile(projectembed.FS, path.Join("projects", dir, clean))
}

// countProjectMigrations returns the number of SQL files under the project's
// migrations/ directory. A missing directory is zero, not an error: a project
// can exist without its own tables (for example an agent-only project that
// uses platform records).
func countProjectMigrations(dir string) int {
	entries, err := fs.ReadDir(projectembed.FS, path.Join("projects", dir, "migrations"))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			n++
		}
	}
	return n
}

// loadSkills reads every .md under the project's skills directory as one
// skill each.
//
// Every skill is always injected, whole. There is no trigger index and no
// selective matching: a project's skills file is the procedures it wants
// followed, and a procedure injected only sometimes is a procedure the model
// follows only sometimes. If the file ever grows noisy enough that always-on
// hurts, split it -- do not add a matching layer on top.
func loadSkills(dir, rel string) ([]Skill, error) {
	clean := path.Clean(strings.TrimPrefix(rel, "/"))
	if clean == "." || strings.HasPrefix(clean, "..") || strings.Contains(clean, "/../") {
		return nil, fmt.Errorf("invalid skills path %q", rel)
	}
	root := path.Join("projects", dir, clean)
	entries, err := fs.ReadDir(projectembed.FS, root)
	if err != nil {
		if isNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s/: %w", root, err)
	}
	var out []Skill
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		b, err := fs.ReadFile(projectembed.FS, path.Join(root, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s/%s: %w", root, e.Name(), err)
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
