package features

import (
	"strings"
	"testing"

	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
)

func TestValidateClientEntitlement(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	Register(Feature{Name: "menu", Kind: "menu", VerticalSlugs: []string{"fnb"}})
	RegisterProject("acme", "sports", Feature{
		Name: "facilities", Kind: "facility", VerticalSlugs: []string{"sports"},
	})
	RegisterProject("acme", "sports", Feature{
		Name: "pricing", Kind: "pricing", VerticalSlugs: []string{"sports"},
	})

	project := &agentconfig.Project{
		Slug: "acme", Vertical: "sports", Schema: "project_acme",
	}
	base := ClientEntitlement{
		TenantID: "acme", DBSchema: "project_acme", Vertical: "sports",
		Features: []string{"records", "facilities", "pricing"}, Status: "active",
	}

	cases := []struct {
		name    string
		client  ClientEntitlement
		project *agentconfig.Project
		want    string // empty means valid
	}{
		{name: "valid project subset", client: base, project: project},
		{
			name:    "empty project entitlement",
			client:  ClientEntitlement{TenantID: "acme", DBSchema: "project_acme", Vertical: "sports"},
			project: project, want: "clients.features is empty",
		},
		{
			name:    "project schema mismatch",
			client:  ClientEntitlement{TenantID: "acme", DBSchema: "public", Vertical: "sports", Features: []string{"records"}},
			project: project, want: "db_schema",
		},
		{
			name:    "project vertical mismatch",
			client:  ClientEntitlement{TenantID: "acme", DBSchema: "project_acme", Vertical: "fnb", Features: []string{"records"}},
			project: project, want: "vertical is",
		},
		{
			name:    "unknown project feature",
			client:  ClientEntitlement{TenantID: "acme", DBSchema: "project_acme", Vertical: "sports", Features: []string{"records", "nope"}},
			project: project, want: "unknown feature",
		},
		{
			name:    "project feature wrong vertical",
			client:  ClientEntitlement{TenantID: "acme", DBSchema: "project_acme", Vertical: "sports", Features: []string{"menu"}},
			project: project, want: "not allowed for vertical",
		},
		{
			name:    "duplicate feature",
			client:  ClientEntitlement{TenantID: "acme", DBSchema: "project_acme", Vertical: "sports", Features: []string{"records", "records"}},
			project: project, want: "duplicated",
		},
		{
			name:    "non-normalized feature",
			client:  ClientEntitlement{TenantID: "acme", DBSchema: "project_acme", Vertical: "sports", Features: []string{"RecordS"}},
			project: project, want: "not normalized",
		},
		{
			name:   "platform empty entitlement allowed",
			client: ClientEntitlement{TenantID: "dfd", DBSchema: "public", Vertical: "agency", Status: "paused"},
		},
		{
			name:   "platform feature wrong vertical",
			client: ClientEntitlement{TenantID: "dfd", DBSchema: "public", Vertical: "sports", Features: []string{"menu"}},
			want:   "not allowed for vertical",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := ValidateClientEntitlement(tc.client, tc.project)
			if tc.want == "" {
				if len(issues) != 0 {
					t.Fatalf("issues = %v, want none", issues)
				}
				return
			}
			joined := strings.Join(issues, "; ")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("issues = %v, want substring %q", issues, tc.want)
			}
		})
	}
}
