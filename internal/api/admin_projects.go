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

// ProjectCatalog is the admin provisioning view of a project manifest plus
// its applied migration ledger. Production uses the boot snapshot; tests
// inject a fixture catalog.
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

// snapshotProjectCatalog is the boot-time manifest snapshot. Production uses
// this so the admin surface, REST router, agent loader, and boot validation
// all answer from the same parsed tree. A project that failed to load is
// returned with its load error, not as "not found": the difference decides
// whether admin treats the tenant as a platform tenant.
type snapshotProjectCatalog struct {
	projects map[string]*agentconfig.Project
	bad      map[string]error
	pool     *pgxpool.Pool
}

func newSnapshotProjectCatalog(projects map[string]*agentconfig.Project, bad map[string]error, pool *pgxpool.Pool) ProjectCatalog {
	return &snapshotProjectCatalog{projects: projects, bad: bad, pool: pool}
}

func (c *snapshotProjectCatalog) Get(slug string) (*agentconfig.Project, error) {
	if p, ok := c.projects[slug]; ok {
		return p, nil
	}
	if err, ok := c.bad[slug]; ok {
		return nil, err
	}
	return nil, ErrProjectNotFound
}

func (c *snapshotProjectCatalog) Ready(ctx context.Context, p *agentconfig.Project) (bool, error) {
	return projectReady(ctx, c.pool, p)
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
	return projectReady(ctx, c.pool, p)
}

// projectReady is the shared schema/ledger check; both the embedded catalog
// and the boot snapshot must use one definition of provisionable.
func projectReady(ctx context.Context, pool *pgxpool.Pool, p *agentconfig.Project) (bool, error) {
	var schemaExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, p.Schema).
		Scan(&schemaExists); err != nil {
		return false, fmt.Errorf("inspect project schema: %w", err)
	}
	if !schemaExists {
		return false, nil
	}
	// A schema that was recreated empty is not provisionable. The ledger can
	// still have rows from the old schema; require the records table the
	// project router always mounts.
	var hasRecords bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass(format('%I.tenant_records', $1::text)) IS NOT NULL`, p.Schema).
		Scan(&hasRecords); err != nil {
		return false, fmt.Errorf("inspect project objects: %w", err)
	}
	if !hasRecords {
		return false, nil
	}
	var applied, otherSchema int
	if err := pool.QueryRow(ctx, `
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
