// Command provision is the single operator path for a new tenant.
//
// It validates projects/<slug>/project.json, creates the client through the
// admin API, and optionally seeds the first owner. The client token and
// generated owner password are printed once.
//
// A project tenant must already have its schema migrated; use `cmd/migrate`
// or the db-migrate workflow first. `--migrate` is an explicit local/staging
// convenience that runs the guarded migrator. A platform tenant (--platform)
// goes to schema public and needs no folder.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
	"github.com/davinakmalyasha/agents-proxy/internal/config"
	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/internal/features"
	_ "github.com/davinakmalyasha/agents-proxy/internal/platformfeatures"
	_ "github.com/davinakmalyasha/agents-proxy/internal/projectsgen"
)

const usage = `usage: provision --slug <slug> --name "<name>" [flags]

  --url / AGENTS_PROXY_URL       admin API base (required)
  --key / ADMIN_API_KEY          admin API key (required)
  --slug                         tenant id / project slug (required)
  --name                         display name (required)
  --platform                     create a platform tenant in public (no folder)
  --vertical                     override/confirm project.json vertical
  --schema                       override/confirm project.json schema
  --quota                        monthly token quota (default 500000)
  --feature                      comma-separated feature names (repeatable)
  --origin                       frontend origin (repeatable)
  --owner-email                  seed the first owner
  --owner-password-stdin         read the owner password from one stdin line
  --generate-owner-password      generate and print an owner password once
  --migrate                      apply migrations through the guarded CLI path
  --dry-run                      print the plan without changing anything

Production DDL normally runs through the db-migrate workflow. --migrate is
for a local/staging database that has not been migrated yet; it validates
ENV, ALLOW_REMOTE_DB, and EXPECTED_DB_REF before opening a connection.
Secrets are printed once and never echoed back.
`

type options struct {
	baseURL    string
	adminKey   string
	slug       string
	name       string
	platform   bool
	vertical   string
	schema     string
	quota      int
	features   csvFlag
	origins    csvFlag
	ownerEmail string
	ownerStdin bool
	ownerGen   bool
	migrate    bool
	dryRun     bool
}

type csvFlag []string

func (c *csvFlag) String() string { return strings.Join(*c, ",") }
func (c *csvFlag) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*c = append(*c, p)
		}
	}
	return nil
}

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var o options
	fs := flag.NewFlagSet("provision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.baseURL, "url", os.Getenv("AGENTS_PROXY_URL"), "admin API base URL")
	fs.StringVar(&o.adminKey, "key", os.Getenv("ADMIN_API_KEY"), "admin API key")
	fs.StringVar(&o.slug, "slug", "", "tenant id / project slug")
	fs.StringVar(&o.name, "name", "", "display name")
	fs.BoolVar(&o.platform, "platform", false, "create a platform tenant in public")
	fs.StringVar(&o.vertical, "vertical", "", "vertical override")
	fs.StringVar(&o.schema, "schema", "", "schema override")
	fs.IntVar(&o.quota, "quota", 0, "monthly token quota")
	fs.Var(&o.features, "feature", "feature name (repeatable or comma-separated)")
	fs.Var(&o.origins, "origin", "frontend origin (repeatable)")
	fs.StringVar(&o.ownerEmail, "owner-email", "", "first owner email")
	fs.BoolVar(&o.ownerStdin, "owner-password-stdin", false, "read owner password from stdin")
	fs.BoolVar(&o.ownerGen, "generate-owner-password", false, "generate an owner password")
	fs.BoolVar(&o.migrate, "migrate", false, "apply migrations through the guarded CLI path")
	fs.BoolVar(&o.dryRun, "dry-run", false, "print the plan only")
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, usage) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "provision: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if !slugRe.MatchString(o.slug) {
		_, _ = fmt.Fprintln(stderr, "provision: --slug is required and must be lowercase letters, digits, and hyphens")
		return 2
	}
	if strings.TrimSpace(o.name) == "" {
		_, _ = fmt.Fprintln(stderr, "provision: --name is required")
		return 2
	}
	if o.ownerStdin && o.ownerGen {
		_, _ = fmt.Fprintln(stderr, "provision: choose --owner-password-stdin or --generate-owner-password, not both")
		return 2
	}
	if o.ownerEmail != "" && !o.ownerStdin && !o.ownerGen {
		_, _ = fmt.Fprintln(stderr, "provision: --owner-email requires --owner-password-stdin or --generate-owner-password")
		return 2
	}
	if o.platform && o.ownerEmail != "" {
		_, _ = fmt.Fprintln(stderr, "provision: a platform tenant has no project staff provider; omit --owner-email")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	project, err := resolveProject(o.slug, o.platform)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "provision: %v\n", err)
		return 1
	}
	vertical, schema, features, err := resolveTenant(o, project)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "provision: %v\n", err)
		return 1
	}

	if o.dryRun {
		_, _ = fmt.Fprintf(stdout, "would provision %s\n  schema:   %s\n  vertical: %s\n  features: %s\n",
			o.slug, schema, vertical, strings.Join(features, ","))
		if o.ownerEmail != "" {
			_, _ = fmt.Fprintf(stdout, "  owner:    %s\n", o.ownerEmail)
		}
		if o.migrate {
			_, _ = fmt.Fprintln(stdout, "  migrate:  yes")
		}
		return 0
	}

	if o.migrate {
		if strings.EqualFold(strings.TrimSpace(os.Getenv("ENV")), "production") {
			_, _ = fmt.Fprintln(stderr, "provision: --migrate is for local/staging; production DDL runs through the db-migrate workflow")
			return 1
		}
		dbCfg, err := config.LoadDatabase()
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "provision: %v\n", err)
			return 1
		}
		adminDSN := dbCfg.DatabaseAdminURL
		pool, err := db.NewPool(ctx, adminDSN)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "provision: connect database: %v\n", err)
			return 1
		}
		if err := db.MigrateAll(ctx, pool); err != nil {
			pool.Close()
			_, _ = fmt.Fprintf(stderr, "provision: migrate: %v\n", err)
			return 1
		}
		status, err := db.InspectMigrations(ctx, pool)
		if err != nil {
			pool.Close()
			_, _ = fmt.Fprintf(stderr, "provision: inspect migrations: %v\n", err)
			return 1
		}
		pool.Close()
		if !status.Clean() {
			_, _ = fmt.Fprintln(stderr, "provision: migrations are not clean; run migrate check")
			return 1
		}
		_, _ = fmt.Fprintln(stderr, "migrations applied and clean")
	}

	c, err := newAdminClient(o.baseURL, o.adminKey)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "provision: %v\n", err)
		return 1
	}
	var created createResponse
	if err := c.do(ctx, http.MethodPost, "/v1/admin/clients", createRequest{
		Name:               strings.TrimSpace(o.name),
		TenantID:           o.slug,
		MonthlyQuotaTokens: o.quota,
		Vertical:           vertical,
		Features:           features,
		Schema:             schema,
		Origins:            o.origins,
	}, &created); err != nil {
		_, _ = fmt.Fprintf(stderr, "provision: create client: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintf(stdout, "created %s (%s)\n", created.TenantID, created.ID)
	_, _ = fmt.Fprintf(stdout, "schema:   %s\n", created.Schema)
	_, _ = fmt.Fprintf(stdout, "vertical: %s\n", created.Vertical)
	_, _ = fmt.Fprintf(stdout, "features: %s\n", strings.Join(created.Features, ","))
	_, _ = fmt.Fprintf(stdout, "token:    %s\n", created.Token)
	_, _ = fmt.Fprintln(stderr, "store the client token now; the admin API will not show it again")

	if o.ownerEmail == "" {
		return 0
	}
	password, err := ownerPassword(o, stdin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "provision: %v\n", err)
		return 1
	}
	var owner struct {
		User struct {
			ID    string `json:"id"`
			Email string `json:"email"`
			Role  string `json:"role"`
		} `json:"user"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/admin/clients/"+created.ID+"/owner", map[string]any{
		"email":     o.ownerEmail,
		"password":  password,
		"full_name": strings.TrimSpace(o.name) + " Owner",
		"role":      "owner",
	}, &owner); err != nil {
		// Do not leave an active ownerless tenant behind. Pausing is the
		// reversible action; the operator can resume after seeding the owner.
		_ = c.do(ctx, http.MethodPatch, "/v1/admin/clients/"+created.ID, map[string]any{"status": "paused"}, nil)
		_, _ = fmt.Fprintf(stderr, "provision: seed owner: %v\n", err)
		_, _ = fmt.Fprintln(stderr, "client paused; seed the owner manually then resume it")
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "owner:    %s (%s)\n", owner.User.Email, owner.User.Role)
	if o.ownerGen {
		_, _ = fmt.Fprintf(stdout, "password: %s\n", password)
		_, _ = fmt.Fprintln(stderr, "store the owner password now; it is not recoverable from the API")
	}
	return 0
}

func resolveProject(slug string, platform bool) (*agentconfig.Project, error) {
	dir, err := agentconfig.DirForSlug(slug)
	if err != nil {
		if platform {
			return nil, nil
		}
		return nil, fmt.Errorf("projects/%s/project.json not found; add the folder or use --platform", slug)
	}
	if platform {
		return nil, fmt.Errorf("%s has a project folder; omit --platform to provision it as a project", slug)
	}
	p, err := agentconfig.LoadSingle(dir)
	if err != nil {
		return nil, err
	}
	if err := agentconfig.Validate(p, agentconfig.DefaultBudget()); err != nil {
		return nil, err
	}
	return p, nil
}

func resolveTenant(o options, project *agentconfig.Project) (vertical, schema string, features []string, err error) {
	if project == nil {
		vertical = strings.TrimSpace(o.vertical)
		if vertical == "" {
			vertical = "fnb"
		}
		schema = strings.TrimSpace(o.schema)
		if schema == "" {
			schema = "public"
		}
		if schema != "public" {
			return "", "", nil, errors.New("a platform tenant must use schema=public")
		}
		feats := append([]string(nil), o.features...)
		if err := validateFeatures(o.slug, vertical, feats); err != nil {
			return "", "", nil, err
		}
		return vertical, schema, feats, nil
	}
	vertical = project.Vertical
	if v := strings.TrimSpace(o.vertical); v != "" {
		if v != project.Vertical {
			return "", "", nil, fmt.Errorf("--vertical %q does not match project.json %q", v, project.Vertical)
		}
		vertical = v
	}
	schema = project.Schema
	if s := strings.TrimSpace(o.schema); s != "" {
		if s != project.Schema {
			return "", "", nil, fmt.Errorf("--schema %q does not match project.json %q", s, project.Schema)
		}
		schema = s
	}
	if len(o.features) > 0 {
		features = append([]string(nil), o.features...)
	} else {
		features = append([]string(nil), project.Features...)
	}
	if err := validateFeatures(project.Slug, vertical, features); err != nil {
		return "", "", nil, err
	}
	return vertical, schema, features, nil
}

func validateFeatures(slug, vertical string, feats []string) error {
	for _, f := range feats {
		if features.IsKnownProjectFeature(slug, f) {
			if !features.ProjectFeatureAllowed(slug, f, vertical) {
				return fmt.Errorf("project feature %q is not allowed for vertical %q", f, vertical)
			}
			continue
		}
		if features.IsKnownFeature(f) {
			if !features.AllowedForVertical(f, vertical) {
				return fmt.Errorf("feature %q is not allowed for vertical %q", f, vertical)
			}
			continue
		}
		return fmt.Errorf("unknown feature %q", f)
	}
	return nil
}

func ownerPassword(o options, stdin io.Reader) (string, error) {
	if o.ownerGen {
		var raw [18]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", fmt.Errorf("generate password: %w", err)
		}
		return hex.EncodeToString(raw[:]), nil
	}
	sc := bufio.NewScanner(io.LimitReader(stdin, 4096))
	if !sc.Scan() {
		return "", errors.New("no owner password on stdin")
	}
	pw := strings.TrimSpace(sc.Text())
	if len(pw) < 8 {
		return "", errors.New("owner password must be at least 8 characters")
	}
	return pw, nil
}

type createRequest struct {
	Name               string   `json:"name"`
	TenantID           string   `json:"tenant_id"`
	MonthlyQuotaTokens int      `json:"monthly_quota_tokens,omitempty"`
	Vertical           string   `json:"vertical,omitempty"`
	Features           []string `json:"features,omitempty"`
	Schema             string   `json:"schema,omitempty"`
	Origins            []string `json:"origins,omitempty"`
}

type createResponse struct {
	ID       string   `json:"id"`
	TenantID string   `json:"tenant_id"`
	Token    string   `json:"token"`
	Vertical string   `json:"vertical"`
	Features []string `json:"features"`
	Schema   string   `json:"schema"`
}

type adminClient struct {
	baseURL string
	key     string
	http    *http.Client
}

func newAdminClient(baseURL, key string) (*adminClient, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	key = strings.TrimSpace(key)
	if base == "" {
		return nil, errors.New("admin URL is required (--url or AGENTS_PROXY_URL)")
	}
	if key == "" {
		return nil, errors.New("admin key is required (--key or ADMIN_API_KEY)")
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return nil, errors.New("invalid admin URL")
	}
	if u.Scheme != "https" && !isLoopback(u.Hostname()) {
		return nil, errors.New("refusing to send the admin key over plain HTTP; use https")
	}
	return &adminClient{baseURL: base, key: key, http: &http.Client{Timeout: 60 * time.Second}}, nil
}

func (c *adminClient) do(ctx context.Context, method, path string, body, out any) error {
	var buf io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		buf = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, buf)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &apiErr)
		msg := strings.TrimSpace(apiErr.Error)
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		return fmt.Errorf("admin API %s: %s", resp.Status, msg)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func isLoopback(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// slugRe is duplicated from the admin client on purpose: the provision command
// validates the id before it touches the network or the migrations.
var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
