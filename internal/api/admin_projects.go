package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
)

// ErrProjectNotFound means no projects/<slug>/ folder is embedded in this
// build. A project schema cannot be provisioned without one.
var ErrProjectNotFound = errors.New("project folder not found")

// ProjectCatalog is the admin provisioning view of the build-time project
// tree plus its applied migration ledger. Production uses the embedded
// folders; tests inject a fixture catalog.
type ProjectCatalog interface {
	// Get returns the project declaration for a tenant slug.
	Get(slug string) (*agentconfig.Project, error)
	// Ready reports whether the project's schema exists and its migration
	// ledger is complete for that schema.
	Ready(ctx context.Context, p *agentconfig.Project) (bool, error)
}

type embeddedProjectCatalog struct {
	pool *pgxpool.Pool
}

func newEmbeddedProjectCatalog(pool *pgxpool.Pool) ProjectCatalog {
	return &embeddedProjectCatalog{pool: pool}
}

func (c *embeddedProjectCatalog) Get(slug string) (*agentconfig.Project, error) {
	dir, err := agentconfig.DirForSlug(slug)
	if err != nil {
		return nil, ErrProjectNotFound
	}
	p, err := agentconfig.LoadSingle(dir)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (c *embeddedProjectCatalog) Ready(ctx context.Context, p *agentconfig.Project) (bool, error) {
	var schemaExists bool
	if err := c.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, p.Schema).
		Scan(&schemaExists); err != nil {
		return false, fmt.Errorf("inspect project schema: %w", err)
	}
	if !schemaExists {
		return false, nil
	}
	var applied, otherSchema int
	if err := c.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE db_schema = $2),
		       count(*) FILTER (WHERE db_schema <> $2)
		  FROM public.project_migrations
		 WHERE project_slug = $1`, p.Slug, p.Schema).
		Scan(&applied, &otherSchema); err != nil {
		return false, fmt.Errorf("inspect project migrations: %w", err)
	}
	if otherSchema > 0 {
		return false, fmt.Errorf("project %q has migrations recorded for another schema", p.Slug)
	}
	if applied != p.Migrations {
		return false, nil
	}
	return true, nil
}
