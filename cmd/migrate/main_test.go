package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/internal/testsafety"
)

// TestProjectMetadataIssuesFlagsEmptyEntitlement pins the deploy guard: an
// active project tenant with no feature list silently loses every gated
// surface, including public landing reads, while /records keeps working and
// hides the mistake. migrate check must fail instead.
func TestProjectMetadataIssuesFlagsEmptyEntitlement(t *testing.T) {
	testsafety.RequireLocal(t)
	dsn := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if dsn == "" {
		dsn = os.Getenv("TEST_DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("TEST_DATABASE_ADMIN_URL / TEST_DATABASE_URL not set — skipping integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run LIFO after the test function returns, so the row delete
	// must be registered AFTER the pool close to run BEFORE it.
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM clients WHERE tenant_id = 'centrum'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
		 VALUES ('Centrum', 'centrum', 'migrate-check', 5000, 'active', 'sports', '{}', 'project_centrum')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM clients WHERE tenant_id = 'centrum'`)
	})

	issues, err := projectMetadataIssues(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, issue := range issues {
		if strings.Contains(issue, "clients.features is empty") {
			return
		}
	}
	t.Fatalf("empty entitlement not reported: %v", issues)
}
