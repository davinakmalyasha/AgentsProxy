// Command tenant provisions and inspects clients through the admin API.
//
// It is a thin operator client: every write goes through the same
// /v1/admin endpoints the dashboard uses, so provisioning rules (schema
// authority, project features, origin validation, token hashing) live in one
// place. The CLI never connects to Postgres directly and never reads .env.
//
// Usage:
//
//	AGENTS_PROXY_URL=https://... ADMIN_API_KEY=... \
//	  go run ./cmd/tenant create --slug acme --name "Acme" --vertical sports
//
//	AGENTS_PROXY_URL=https://... ADMIN_API_KEY=... go run ./cmd/tenant list
//
// The client token returned by create is shown once. Store it immediately.
package main

import (
	"bytes"
	"context"
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
	"text/tabwriter"
	"time"
)

const usage = `usage: tenant <create|list|patch|usage> [flags]

Global flags:
  --url   admin API base (default $AGENTS_PROXY_URL)
  --key   admin API key  (default $ADMIN_API_KEY)

Commands:
  create  create a client and print its token once
  list    list clients
  patch   update quota, status, vertical, or features
  usage   show token usage for a period

A project tenant requires projects/<slug>/project.json and its schema must
already be migrated. --platform creates a platform tenant in public instead.
`

var (
	slugRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	schemaRe = regexp.MustCompile(`^project_[a-z0-9_]{1,55}$`)
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	command := args[0]
	args = args[1:]

	switch command {
	case "create":
		return runCreate(args, stdout, stderr)
	case "list":
		return runList(args, stdout, stderr)
	case "patch":
		return runPatch(args, stdout, stderr)
	case "usage":
		return runUsage(args, stdout, stderr)
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "tenant: unknown command %q\n\n%s", command, usage)
		return 2
	}
}

type globals struct {
	baseURL string
	key     string
	json    bool
}

func addGlobals(fs *flag.FlagSet) *globals {
	g := &globals{}
	fs.StringVar(&g.baseURL, "url", os.Getenv("AGENTS_PROXY_URL"), "admin API base URL")
	fs.StringVar(&g.key, "key", os.Getenv("ADMIN_API_KEY"), "admin API key")
	fs.BoolVar(&g.json, "json", false, "emit the raw JSON response")
	return g
}

func (g *globals) client() (*apiClient, error) {
	base := strings.TrimRight(strings.TrimSpace(g.baseURL), "/")
	key := strings.TrimSpace(g.key)
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
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return nil, errors.New("refusing to send the admin key over plain HTTP; use https")
	}
	return &apiClient{
		baseURL: base,
		key:     key,
		http:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func runCreate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tenant create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	g := addGlobals(fs)
	slug := fs.String("slug", "", "tenant id (required)")
	name := fs.String("name", "", "display name (required)")
	vertical := fs.String("vertical", "", "vertical (defaults to project.json or fnb)")
	schema := fs.String("schema", "", "project schema (defaults to project.json or project_<slug>)")
	quota := fs.Int("quota", 0, "monthly token quota (default 500000)")
	features := fs.String("features", "", "comma-separated feature names (default: server default set)")
	origins := &csvFlag{}
	fs.Var(origins, "origin", "frontend origin (repeatable)")
	platform := fs.Bool("platform", false, "create a platform tenant in public with no projects/<slug> folder")
	allowMissing := fs.Bool("allow-missing-project", false, "deprecated; use --platform")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *allowMissing {
		*platform = true
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "tenant create: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if !slugRe.MatchString(*slug) {
		_, _ = fmt.Fprintln(stderr, "tenant create: --slug is required and must be lowercase letters, digits, and hyphens")
		return 2
	}
	if strings.TrimSpace(*name) == "" {
		_, _ = fmt.Fprintln(stderr, "tenant create: --name is required")
		return 2
	}

	doc, err := readProjectDoc(*slug, !*platform)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "tenant create: %v\n", err)
		return 1
	}
	if *platform {
		if doc != nil {
			_, _ = fmt.Fprintf(stderr,
				"tenant create: %s has a project folder; omit --platform to provision it as a project\n", *slug)
			return 1
		}
		if *schema != "" && *schema != "public" {
			_, _ = fmt.Fprintln(stderr, "tenant create: --platform requires schema=public")
			return 1
		}
		*schema = "public"
	} else if doc != nil {
		if *vertical == "" {
			*vertical = doc.Vertical
		}
		if *schema == "" {
			*schema = doc.Schema
		}
	}

	c, err := g.client()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "tenant create: %v\n", err)
		return 1
	}

	var featureList []string
	if strings.TrimSpace(*features) != "" {
		featureList = splitCSV(*features)
	}
	req := createRequest{
		Name:               strings.TrimSpace(*name),
		TenantID:           *slug,
		MonthlyQuotaTokens: *quota,
		Vertical:           *vertical,
		Features:           featureList,
		Schema:             *schema,
		Origins:            origins.values,
	}
	var resp createResponse
	if err := c.do(context.Background(), http.MethodPost, "/v1/admin/clients", req, &resp); err != nil {
		_, _ = fmt.Fprintf(stderr, "tenant create: %v\n", err)
		return 1
	}

	if g.json {
		return writeJSON(stdout, stderr, resp)
	}
	_, _ = fmt.Fprintf(stdout, "created %s (%s)\n", resp.TenantID, resp.ID)
	_, _ = fmt.Fprintf(stdout, "schema:   %s\n", resp.Schema)
	_, _ = fmt.Fprintf(stdout, "vertical: %s\n", resp.Vertical)
	_, _ = fmt.Fprintf(stdout, "features: %s\n", strings.Join(resp.Features, ","))
	_, _ = fmt.Fprintf(stdout, "token:    %s\n", resp.Token)
	_, _ = fmt.Fprintln(stderr, "store the token now; the admin API will not show it again")
	return 0
}

func runList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tenant list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	g := addGlobals(fs)
	limit := fs.Int("limit", 200, "maximum rows (1-2000)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "tenant list: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *limit < 1 || *limit > 2000 {
		_, _ = fmt.Fprintln(stderr, "tenant list: --limit must be between 1 and 2000")
		return 2
	}
	c, err := g.client()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "tenant list: %v\n", err)
		return 1
	}
	var rows []clientRow
	if err := c.do(context.Background(), http.MethodGet,
		fmt.Sprintf("/v1/admin/clients?limit=%d", *limit), nil, &rows); err != nil {
		_, _ = fmt.Fprintf(stderr, "tenant list: %v\n", err)
		return 1
	}
	if g.json {
		return writeJSON(stdout, stderr, rows)
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TENANT\tNAME\tVERTICAL\tSTATUS\tSCHEMA\tQUOTA\tFEATURES\tID")
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			r.TenantID, r.Name, r.Vertical, r.Status, r.Schema, r.MonthlyQuotaTokens,
			strings.Join(r.Features, ","), r.ID)
	}
	_ = tw.Flush()
	return 0
}

func runPatch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tenant patch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	g := addGlobals(fs)
	tenant := fs.String("tenant", "", "tenant id (preferred)")
	id := fs.String("id", "", "client UUID (alternative to --tenant)")
	quota := fs.Int("quota", 0, "new monthly token quota")
	status := fs.String("status", "", "active, paused, or blocked")
	vertical := fs.String("vertical", "", "new vertical")
	features := &stringFlag{}
	fs.Var(features, "features", "comma-separated features (empty clears)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "tenant patch: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *tenant == "" && *id == "" {
		_, _ = fmt.Fprintln(stderr, "tenant patch: --tenant or --id is required")
		return 2
	}
	if !quotaSet(fs, "quota") && *status == "" && *vertical == "" && !features.set {
		_, _ = fmt.Fprintln(stderr, "tenant patch: at least one of --quota, --status, --vertical, --features is required")
		return 2
	}
	c, err := g.client()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "tenant patch: %v\n", err)
		return 1
	}
	target := strings.TrimSpace(*id)
	if target == "" {
		target, err = c.resolveID(context.Background(), strings.TrimSpace(*tenant))
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "tenant patch: %v\n", err)
			return 1
		}
	}
	req := patchRequest{}
	if quotaSet(fs, "quota") {
		req.MonthlyQuotaTokens = quota
	}
	if *status != "" {
		req.Status = status
	}
	if *vertical != "" {
		req.Vertical = vertical
	}
	if features.set {
		v := splitCSV(features.value)
		if v == nil {
			v = []string{}
		}
		req.Features = &v
	}
	if err := c.do(context.Background(), http.MethodPatch, "/v1/admin/clients/"+target, req, nil); err != nil {
		_, _ = fmt.Fprintf(stderr, "tenant patch: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "updated %s\n", target)
	return 0
}

func runUsage(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tenant usage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	g := addGlobals(fs)
	tenant := fs.String("tenant", "", "filter to one tenant id")
	from := fs.String("from", "", "RFC3339 start (default: now-30d)")
	to := fs.String("to", "", "RFC3339 end (default: now)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "tenant usage: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	c, err := g.client()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "tenant usage: %v\n", err)
		return 1
	}
	q := url.Values{}
	if *tenant != "" {
		q.Set("client", *tenant)
	}
	if *from != "" {
		if _, err := time.Parse(time.RFC3339, *from); err != nil {
			_, _ = fmt.Fprintln(stderr, "tenant usage: --from must be RFC3339")
			return 2
		}
		q.Set("from", *from)
	}
	if *to != "" {
		if _, err := time.Parse(time.RFC3339, *to); err != nil {
			_, _ = fmt.Fprintln(stderr, "tenant usage: --to must be RFC3339")
			return 2
		}
		q.Set("to", *to)
	}
	path := "/v1/admin/usage"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var rows []usageRow
	if err := c.do(context.Background(), http.MethodGet, path, nil, &rows); err != nil {
		_, _ = fmt.Fprintf(stderr, "tenant usage: %v\n", err)
		return 1
	}
	if g.json {
		return writeJSON(stdout, stderr, rows)
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TENANT\tNAME\tIN\tOUT\tCOST_USD")
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%.4f\n",
			r.TenantID, r.Name, r.TokensIn, r.TokensOut, r.CostUSD)
	}
	_ = tw.Flush()
	return 0
}

func quotaSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

type csvFlag struct {
	values []string
}

func (c *csvFlag) String() string { return strings.Join(c.values, ",") }

func (c *csvFlag) Set(v string) error {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	c.values = append(c.values, strings.TrimSpace(v))
	return nil
}

type stringFlag struct {
	value string
	set   bool
}

func (s *stringFlag) String() string { return s.value }

func (s *stringFlag) Set(v string) error {
	s.value = v
	s.set = true
	return nil
}

func splitCSV(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

type projectDoc struct {
	Slug     string `json:"slug"`
	Vertical string `json:"vertical"`
	Schema   string `json:"schema"`
}

func readProjectDoc(slug string, required bool) (*projectDoc, error) {
	if !slugRe.MatchString(slug) {
		return nil, errors.New("invalid slug")
	}
	path := "projects/" + slug + "/project.json"
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if required {
				return nil, fmt.Errorf("%s not found (create the project folder first)", path)
			}
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc projectDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	if doc.Slug != "" && doc.Slug != slug {
		return nil, fmt.Errorf("%s declares slug %q", path, doc.Slug)
	}
	if doc.Schema == "" {
		doc.Schema = "project_" + strings.ReplaceAll(slug, "-", "_")
	}
	if !schemaRe.MatchString(doc.Schema) {
		return nil, fmt.Errorf("%s declares schema %q; project schemas must match project_<slug> (max 63 characters)", path, doc.Schema)
	}
	return &doc, nil
}

type apiClient struct {
	baseURL string
	key     string
	http    *http.Client
}

func (c *apiClient) do(ctx context.Context, method, path string, body, out any) error {
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
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode admin response: %w", err)
	}
	return nil
}

func (c *apiClient) resolveID(ctx context.Context, tenant string) (string, error) {
	if tenant == "" || !slugRe.MatchString(tenant) {
		return "", errors.New("invalid tenant id")
	}
	var rows []clientRow
	if err := c.do(ctx, http.MethodGet, "/v1/admin/clients?limit=2000", nil, &rows); err != nil {
		return "", err
	}
	for _, r := range rows {
		if r.TenantID == tenant {
			return r.ID, nil
		}
	}
	return "", fmt.Errorf("tenant %q not found", tenant)
}

func writeJSON(stdout, stderr io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		_, _ = fmt.Fprintf(stderr, "encode response: %v\n", err)
		return 1
	}
	return 0
}

func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

type createRequest struct {
	Name               string   `json:"name"`
	TenantID           string   `json:"tenant_id"`
	MonthlyQuotaTokens int      `json:"monthly_quota_tokens,omitempty"`
	Vertical           string   `json:"vertical"`
	Features           []string `json:"features,omitempty"`
	Schema             string   `json:"schema,omitempty"`
	Origins            []string `json:"origins,omitempty"`
}

type createResponse struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	TenantID           string   `json:"tenant_id"`
	Token              string   `json:"token"`
	MonthlyQuotaTokens int      `json:"monthly_quota_tokens"`
	Vertical           string   `json:"vertical"`
	Features           []string `json:"features"`
	Schema             string   `json:"schema"`
}

type clientRow struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	TenantID           string   `json:"tenant_id"`
	Vertical           string   `json:"vertical"`
	Features           []string `json:"features"`
	Status             string   `json:"status"`
	MonthlyQuotaTokens int      `json:"monthly_quota_tokens"`
	Schema             string   `json:"schema"`
}

type patchRequest struct {
	MonthlyQuotaTokens *int      `json:"monthly_quota_tokens,omitempty"`
	Status             *string   `json:"status,omitempty"`
	Vertical           *string   `json:"vertical,omitempty"`
	Features           *[]string `json:"features,omitempty"`
}

type usageRow struct {
	TenantID  string  `json:"tenant_id"`
	Name      string  `json:"name"`
	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`
}
