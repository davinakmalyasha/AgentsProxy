package features

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/internal/testsafety"
)

// TestAuditProjectEntitlementsFencesEmptyProject is the boot-side regression
// for the Centrum failure: a project tenant with an empty live entitlement
// must be named for quarantine, while a platform tenant with an empty set
// stays legal.
func TestAuditProjectEntitlementsFencesEmptyProject(t *testing.T) {
	testsafety.RequireLocal(t)
	dsn := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if dsn == "" {
		dsn = os.Getenv("TEST_DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	cleanup := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM clients WHERE tenant_id IN ('audit-project','audit-platform')`)
	}
	cleanup()
	t.Cleanup(cleanup)
	if _, err := pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
		 VALUES ('Audit Project','audit-project','audit-project',1,'active','sports','{}','project_centrum'),
		        ('Audit Platform','audit-platform','audit-platform',1,'active','agency','{}','public')`); err != nil {
		t.Fatal(err)
	}

	projects := map[string]*agentconfig.Project{
		"audit-project": {Slug: "audit-project", Schema: "project_centrum", Vertical: "sports"},
	}
	fenced, err := AuditProjectEntitlements(ctx, pool, projects, nil)
	if err != nil {
		t.Fatal(err)
	}
	reason, ok := fenced["audit-project"]
	if !ok || !strings.Contains(reason, "clients.features is empty") {
		t.Fatalf("audit-project fence = %q, want empty-entitlement reason", reason)
	}
	if _, ok := fenced["audit-platform"]; ok {
		t.Fatalf("paused platform tenant was fenced: %v", fenced)
	}
}
