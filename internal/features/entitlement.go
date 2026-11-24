package features

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
)

// AuditProjectEntitlements validates every active client row and returns a
// slug -> reason map for the tenants that must be fenced. Projects already
// fenced for a manifest/agent-config failure are skipped: their existing
// reason is the one an operator needs to see.
//
// This is the boot-side counterpart to cmd/migrate check. It runs on every
// process start, including serverless cold starts, so a manual database edit
// after a deploy cannot silently turn a healthy tenant into 403s.
func AuditProjectEntitlements(
	ctx context.Context,
	pool *pgxpool.Pool,
	projects map[string]*agentconfig.Project,
	skip map[string]string,
) (map[string]string, error) {
	clients, err := LoadClientEntitlements(ctx, pool)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, c := range clients {
		if c.Status != "active" {
			continue
		}
		if _, already := skip[c.TenantID]; already {
			continue
		}
		issues := ValidateClientEntitlement(c, projects[c.TenantID])
		if len(issues) == 0 {
			continue
		}
		out[c.TenantID] = strings.Join(issues, "; ")
	}
	return out, nil
}

// ClientEntitlement is the runtime row that decides whether a tenant's
// features are reachable. It is deliberately a plain value so the boot audit,
// cmd/migrate, and the admin reload path can validate the same data without
// depending on a project package.
type ClientEntitlement struct {
	TenantID string
	DBSchema string
	Vertical string
	Features []string
	Status   string
}

// LoadClientEntitlements reads every client row, including paused tenants.
// Paused tenants are included so cmd/migrate can report bad data before the
// tenant is resumed; callers that serve traffic filter on Status.
func LoadClientEntitlements(ctx context.Context, pool *pgxpool.Pool) ([]ClientEntitlement, error) {
	rows, err := pool.Query(ctx,
		`SELECT tenant_id, db_schema, vertical, features, status
		   FROM public.clients
		  ORDER BY tenant_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ClientEntitlement, 0, 16)
	for rows.Next() {
		var c ClientEntitlement
		if err := rows.Scan(&c.TenantID, &c.DBSchema, &c.Vertical, &c.Features, &c.Status); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ValidateClientEntitlement checks one clients row against the compiled
// registry and, for project tenants, the manifest that owns the schema and
// vertical. It returns human-readable issues; an empty slice means the row is
// safe to serve.
//
// A nil project means a platform tenant. Platform tenants may have an empty
// feature set (paused tenants such as dfd-agency are the live example).
// Project tenants may not: an empty list denies every gated feature,
// including public landing reads, while the always-on records surface keeps
// answering, which is exactly the silent outage this guard exists to catch.
func ValidateClientEntitlement(c ClientEntitlement, project *agentconfig.Project) []string {
	issues := make([]string, 0, 4)

	if project == nil {
		for _, raw := range c.Features {
			name := strings.ToLower(strings.TrimSpace(raw))
			if name != raw {
				issues = append(issues, fmt.Sprintf("feature %q is not normalized", raw))
			}
			if !IsKnownFeature(name) || !AllowedForVertical(name, c.Vertical) {
				issues = append(issues, fmt.Sprintf("feature %q is not allowed for vertical %q", raw, c.Vertical))
			}
		}
		return issues
	}

	if project.Schema != "" && c.DBSchema != project.Schema {
		issues = append(issues, fmt.Sprintf(
			"clients.db_schema is %q but project.json declares %q", c.DBSchema, project.Schema))
	}
	if project.Vertical != "" && c.Vertical != project.Vertical {
		issues = append(issues, fmt.Sprintf(
			"clients.vertical is %q but project.json declares %q", c.Vertical, project.Vertical))
	}
	if len(c.Features) == 0 {
		issues = append(issues, "clients.features is empty; gated features are denied (set at least the project.json defaults)")
	}
	if project.StrictFeatures {
		have := make(map[string]bool, len(c.Features))
		for _, f := range c.Features {
			have[strings.ToLower(strings.TrimSpace(f))] = true
		}
		for _, want := range project.Features {
			name := strings.ToLower(strings.TrimSpace(want))
			if name == "" || have[name] {
				continue
			}
			issues = append(issues, fmt.Sprintf(
				"required feature %q is missing from clients.features (strict_features=true)", want))
		}
	}

	seen := make(map[string]bool, len(c.Features))
	for _, raw := range c.Features {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name != raw {
			issues = append(issues, fmt.Sprintf("feature %q is not normalized", raw))
		}
		if name == "" {
			issues = append(issues, "clients.features contains an empty name")
			continue
		}
		if seen[name] {
			issues = append(issues, fmt.Sprintf("feature %q is duplicated", raw))
			continue
		}
		seen[name] = true

		switch {
		case IsKnownProjectFeature(c.TenantID, name):
			if !ProjectFeatureAllowed(c.TenantID, name, c.Vertical) {
				issues = append(issues, fmt.Sprintf(
					"feature %q is not allowed for vertical %q", raw, c.Vertical))
			}
		case IsKnownFeature(name):
			if !AllowedForVertical(name, c.Vertical) {
				issues = append(issues, fmt.Sprintf(
					"feature %q is not allowed for vertical %q", raw, c.Vertical))
			}
		default:
			issues = append(issues, fmt.Sprintf("unknown feature %q", raw))
		}
	}
	sort.Strings(issues)
	return issues
}
