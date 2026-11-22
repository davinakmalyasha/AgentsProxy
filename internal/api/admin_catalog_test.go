package api

import (
	"context"

	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
)

// fakeProjectCatalog is the provisioning view used by admin tests. It lets a
// test declare a project tenant without adding a folder to the embedded
// build; production uses embeddedProjectCatalog.
type fakeProjectCatalog struct {
	projects map[string]*agentconfig.Project
	ready    map[string]bool
	getErr   map[string]error
	readyErr map[string]error
}

func newFakeProjectCatalog() *fakeProjectCatalog {
	return &fakeProjectCatalog{
		projects: map[string]*agentconfig.Project{},
		ready:    map[string]bool{},
		getErr:   map[string]error{},
		readyErr: map[string]error{},
	}
}

func (f *fakeProjectCatalog) add(slug, schema, vertical string, features []string, migrations int, ready bool) {
	f.projects[slug] = &agentconfig.Project{
		Slug:       slug,
		Schema:     schema,
		Vertical:   vertical,
		Features:   features,
		Migrations: migrations,
	}
	f.ready[slug] = ready
}

func (f *fakeProjectCatalog) Get(slug string) (*agentconfig.Project, error) {
	if err := f.getErr[slug]; err != nil {
		return nil, err
	}
	p, ok := f.projects[slug]
	if !ok {
		return nil, ErrProjectNotFound
	}
	return p, nil
}

func (f *fakeProjectCatalog) Ready(_ context.Context, p *agentconfig.Project) (bool, error) {
	if err := f.readyErr[p.Slug]; err != nil {
		return false, err
	}
	return f.ready[p.Slug], nil
}
