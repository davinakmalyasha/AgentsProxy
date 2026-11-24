// Command migrate applies the embedded SQL migrations and exits.
//
// It is the deploy-step runner for environments where the server boots
// with RUN_MIGRATIONS=false. Vercel preview and production refuse boot-time
// migrations; production runs this command through the manual db-migrate
// workflow, gated by the GitHub `production` Environment. Long-running
// hosts (VPS/Railway/compose) may keep boot migrations instead.
//
// Usage:
//
//	DATABASE_URL=... DATABASE_ADMIN_URL=... go run ./cmd/migrate
//	DATABASE_ADMIN_URL=... go run ./cmd/migrate status
//	DATABASE_ADMIN_URL=... go run ./cmd/migrate check
//	DATABASE_ADMIN_URL=... go run ./cmd/migrate plan
//	# or: make migrate (loads the local .env; localhost only)
//
// status, check, and plan are read-only. They never create the ledger
// tables, never apply SQL, and are safe to point at production.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
	"github.com/davinakmalyasha/agents-proxy/internal/config"
	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/internal/features"
	_ "github.com/davinakmalyasha/agents-proxy/internal/platformfeatures"
	_ "github.com/davinakmalyasha/agents-proxy/internal/projectsgen"
)

const usage = `usage: migrate [apply|status|check|plan] [--json]

  apply   apply platform + project migrations (default)
  status  show applied, pending, drift, and missing migrations
  check   exit non-zero unless every migration is applied and unchanged
  plan    show only migrations that apply would still run
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	command := "apply"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		command = args[0]
		args = args[1:]
	}

	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOut := flags.Bool("json", false, "emit machine-readable JSON")
	flags.Usage = func() {
		_, _ = fmt.Fprint(stderr, usage)
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "migrate: unexpected argument %q\n\n%s", flags.Arg(0), usage)
		return 2
	}

	switch command {
	case "apply", "status", "check", "plan":
	default:
		_, _ = fmt.Fprintf(stderr, "migrate: unknown command %q\n\n%s", command, usage)
		return 2
	}

	cfg, err := config.LoadDatabase()
	if err != nil {
		slog.Error("config load", "err", err)
		return 1
	}
	dsn := cfg.DatabaseAdminURL
	if dsn == "" {
		dsn = cfg.DatabaseURL
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool, err := db.NewPoolWithOptions(ctx, dsn, db.NewPoolOptions{
		MinConns:        cfg.DBAdminPoolMin,
		MaxConns:        cfg.DBAdminPoolMax,
		MaxConnIdleTime: time.Minute,
		ApplicationName: "agentsproxy/migrate",
	})
	if err != nil {
		slog.Error("db connect", "err", err)
		return 1
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		slog.Error("db ping", "err", err)
		return 1
	}

	if command == "apply" {
		if err := db.MigrateAll(ctx, pool); err != nil {
			slog.Error("migrate", "err", err)
			return 1
		}
		slog.Info("migrations applied")
		return 0
	}

	status, err := db.InspectMigrations(ctx, pool)
	if err != nil {
		slog.Error("inspect migrations", "err", err)
		return 1
	}
	metadataIssues, err := projectMetadataIssues(ctx, pool)
	if err != nil {
		slog.Error("inspect project metadata", "err", err)
		return 1
	}
	status.Issues = append(status.Issues, metadataIssues...)

	if *jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(status); err != nil {
			slog.Error("encode status", "err", err)
			return 1
		}
	} else {
		switch command {
		case "status":
			printStatus(stdout, status)
		case "plan":
			printPlan(stdout, status)
		case "check":
			printCheck(stdout, status)
		}
	}

	if command == "check" && !status.Clean() {
		return 1
	}
	return 0
}

// projectMetadataIssues reconciles the build-time project declaration with
// the runtime client row. The schema/ledger checks already live in
// db.InspectMigrations; this adds the fields that are not part of a
// migration: vertical and feature entitlement.
func projectMetadataIssues(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	projects, bad := agentconfig.LoadCollect()
	issues := make([]string, 0, len(bad))
	for slug, err := range bad {
		issues = append(issues, fmt.Sprintf("project %q: %v", slug, err))
	}
	for slug, p := range projects {
		if err := agentconfig.Validate(p, agentconfig.DefaultBudget()); err != nil {
			issues = append(issues, fmt.Sprintf("project %q: %v", slug, err))
		}
	}
	for slug, err := range features.ValidateProjectDeclarations(projects) {
		issues = append(issues, fmt.Sprintf("project %q: %v", slug, err))
	}
	for _, slug := range features.UnmanifestedProjects(projects, bad) {
		issues = append(issues, fmt.Sprintf(
			"project %q has compiled features but no projects/%s/project.json", slug, slug))
	}

	clients, err := features.LoadClientEntitlements(ctx, pool)
	if err != nil {
		return nil, err
	}
	for _, c := range clients {
		p, ok := projects[c.TenantID]
		if !ok {
			// A client without a folder is a platform tenant (or the
			// undeclared-schema issue db already reports). Platform
			// tenants may only enable global features allowed for their
			// vertical, and may legitimately have an empty set.
			for _, issue := range features.ValidateClientEntitlement(c, nil) {
				issues = append(issues, fmt.Sprintf("client %q: %s", c.TenantID, issue))
			}
			continue
		}
		if p.Schema != c.DBSchema {
			// db.InspectMigrations reports the schema mismatch with the
			// project ledger context; do not duplicate it.
			continue
		}
		// An empty entitlement is almost always an un-migrated row: every
		// gated feature is denied, including public landing reads, while the
		// always-on records surface still works and hides the problem. The
		// shared validator also catches unknown, duplicate, or vertical-
		// mismatched names so boot and migrate check cannot drift.
		for _, issue := range features.ValidateClientEntitlement(c, p) {
			issues = append(issues, fmt.Sprintf("project %q: %s", c.TenantID, issue))
		}
	}
	sort.Strings(issues)
	return issues, nil
}

func printStatus(w io.Writer, s *db.MigrationStatus) {
	_, _ = fmt.Fprintf(w, "platform ledger: %s\n", initialized(s.PlatformInitialized))
	printEntries(w, "  ", s.Platform)
	for _, p := range s.Projects {
		autoGrant := "true"
		if !p.AutoGrant {
			autoGrant = "false"
		}
		_, _ = fmt.Fprintf(w, "\nproject %s (schema %s, auto_grant=%s)\n", p.Slug, p.Schema, autoGrant)
		printEntries(w, "  ", p.Migrations)
		for _, issue := range p.Issues {
			_, _ = fmt.Fprintf(w, "  ISSUE %s\n", issue)
		}
	}
	for _, issue := range s.Issues {
		_, _ = fmt.Fprintf(w, "\nISSUE %s\n", issue)
	}
	_, _ = fmt.Fprintf(w, "\nsummary: pending=%d issues=%d\n", s.Pending, len(s.Issues))
}

func printPlan(w io.Writer, s *db.MigrationStatus) {
	wrote := false
	printPending := func(prefix string, entries []db.MigrationEntry) {
		for _, e := range entries {
			if e.State == db.MigrationApplied {
				continue
			}
			wrote = true
			_, _ = fmt.Fprintf(w, "%-8s %s%s  %s\n", e.State, prefix, e.Version, checksumLabel(e.FileChecksum))
		}
	}
	printPending("", s.Platform)
	for _, p := range s.Projects {
		printPending("project/"+p.Slug+"/", p.Migrations)
	}
	if !wrote {
		_, _ = fmt.Fprintln(w, "no pending migrations")
	}
}

func printCheck(w io.Writer, s *db.MigrationStatus) {
	for _, entry := range s.Platform {
		if entry.State == db.MigrationDrift || entry.State == db.MigrationMissing {
			_, _ = fmt.Fprintf(w, "platform %s: %s\n", entry.State, entry.Version)
		}
	}
	for _, p := range s.Projects {
		for _, entry := range p.Migrations {
			if entry.State == db.MigrationDrift || entry.State == db.MigrationMissing {
				_, _ = fmt.Fprintf(w, "project %s %s: %s\n", p.Slug, entry.State, entry.Version)
			}
		}
	}
	for _, issue := range s.Issues {
		_, _ = fmt.Fprintf(w, "issue: %s\n", issue)
	}
	if s.Pending > 0 {
		_, _ = fmt.Fprintf(w, "%d migration(s) pending\n", s.Pending)
	}
	if s.Clean() {
		_, _ = fmt.Fprintln(w, "migrations clean")
		return
	}
	_, _ = fmt.Fprintln(w, "migrations are not clean")
}

func printEntries(w io.Writer, indent string, entries []db.MigrationEntry) {
	if len(entries) == 0 {
		_, _ = fmt.Fprintf(w, "%s(no files)\n", indent)
		return
	}
	for _, e := range entries {
		stamp := ""
		if !e.AppliedAt.IsZero() {
			stamp = e.AppliedAt.UTC().Format(time.RFC3339)
		}
		_, _ = fmt.Fprintf(w, "%s%-8s %s%s\n", indent, e.State, e.Version, suffix(stamp))
	}
}

func suffix(v string) string {
	if v == "" {
		return ""
	}
	return "  " + v
}

func checksumLabel(sum string) string {
	if len(sum) > 12 {
		sum = sum[:12]
	}
	if sum == "" {
		return "-"
	}
	return "sha256:" + sum
}

func initialized(v bool) string {
	if v {
		return "present"
	}
	return "not initialized"
}
