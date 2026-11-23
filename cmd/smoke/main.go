// Command smoke is the production contract check shared by the deploy
// workflow, the scheduled uptime workflow, and local preflight.
//
// It deliberately checks the surfaces that broke in production before:
// health + version, Centrum public landing reads, the unauthenticated
// records boundary, the retired platform alias, CORS preflight, and (when an
// admin key is supplied) the tenant quarantine list. A 403/503 from a project
// feature is a failure even though the process is alive.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type options struct {
	baseURL      string
	tenant       string
	legacyVert   string
	expectVer    string
	origin       string
	bypassSecret string
	adminKey     string
	strictCORS   bool
	checkLegacy  bool
	attempts     int
	retryDelay   time.Duration
	timeout      time.Duration
	jsonOut      bool
}

type result struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Status  int    `json:"status,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Elapsed int64  `json:"elapsed_ms"`
}

func run(args []string, stdout, stderr io.Writer) int {
	var o options
	fs := flag.NewFlagSet("smoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.baseURL, "base-url", os.Getenv("SMOKE_BASE_URL"), "base URL to probe (required)")
	fs.StringVar(&o.tenant, "tenant", "centrum", "project tenant slug")
	fs.StringVar(&o.legacyVert, "legacy-vertical", "sports", "vertical for the retired /v1/<vertical> alias check")
	fs.StringVar(&o.expectVer, "expect-version", "", "exact /healthz version (deploy gate)")
	fs.StringVar(&o.origin, "origin", "https://centrum-badminton.vercel.app", "browser origin for the CORS preflight")
	fs.StringVar(&o.bypassSecret, "bypass-secret", os.Getenv("VERCEL_AUTOMATION_BYPASS_SECRET"), "Vercel deployment-protection bypass secret")
	fs.StringVar(&o.adminKey, "admin-key", os.Getenv("ADMIN_API_KEY"), "admin API key for the tenant-health check")
	fs.BoolVar(&o.strictCORS, "strict-cors", true, "reject a wildcard CORS response")
	fs.BoolVar(&o.checkLegacy, "check-legacy", true, "assert the old /v1/<vertical> alias is 404")
	fs.IntVar(&o.attempts, "attempts", 3, "attempts per check")
	fs.DurationVar(&o.retryDelay, "retry-delay", 5*time.Second, "delay between attempts")
	fs.DurationVar(&o.timeout, "timeout", 25*time.Second, "per-request timeout")
	fs.BoolVar(&o.jsonOut, "json", false, "emit JSON results")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	o.baseURL = strings.TrimRight(strings.TrimSpace(o.baseURL), "/")
	if o.baseURL == "" {
		fmt.Fprintln(stderr, "smoke: -base-url or SMOKE_BASE_URL is required")
		return 2
	}
	if o.attempts < 1 {
		o.attempts = 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(o.attempts)*o.timeout+time.Duration(o.attempts)*o.retryDelay+30*time.Second)
	defer cancel()

	results := []result{
		retry(ctx, &o, "health", func() result { return checkHealth(ctx, &o) }),
		retry(ctx, &o, "facilities", func() result {
			return checkJSON(ctx, &o, http.MethodGet, fmt.Sprintf("/v1/p/%s/facilities", o.tenant), nil, http.StatusOK)
		}),
		retry(ctx, &o, "availability", func() result {
			return checkJSON(ctx, &o, http.MethodGet,
				fmt.Sprintf("/v1/p/%s/availability?date=%s", o.tenant, time.Now().UTC().Format("2006-01-02")), nil, http.StatusOK)
		}),
		retry(ctx, &o, "pricing", func() result {
			return checkJSON(ctx, &o, http.MethodGet, fmt.Sprintf("/v1/p/%s/pricing", o.tenant), nil, http.StatusOK)
		}),
		retry(ctx, &o, "records-unauthenticated", func() result {
			return checkJSON(ctx, &o, http.MethodGet, fmt.Sprintf("/v1/p/%s/records", o.tenant), nil, http.StatusUnauthorized)
		}),
	}
	if o.checkLegacy {
		results = append(results, retry(ctx, &o, "legacy-alias-retired", func() result {
			return checkJSON(ctx, &o, http.MethodGet,
				fmt.Sprintf("/v1/%s/facilities?tenant_id=%s", o.legacyVert, o.tenant), nil, http.StatusNotFound)
		}))
	}
	results = append(results, retry(ctx, &o, "cors-preflight", func() result { return checkCORS(ctx, &o) }))
	if strings.TrimSpace(o.adminKey) != "" {
		results = append(results, retry(ctx, &o, "tenant-health", func() result { return checkTenantHealth(ctx, &o) }))
	}

	if o.jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(results)
	} else {
		for _, r := range results {
			mark := "PASS"
			if !r.OK {
				mark = "FAIL"
			}
			line := fmt.Sprintf("%s %-24s", mark, r.Name)
			if r.Status != 0 {
				line += fmt.Sprintf(" http=%d", r.Status)
			}
			if r.Detail != "" {
				line += " " + r.Detail
			}
			fmt.Fprintln(stdout, line)
		}
	}

	for _, r := range results {
		if !r.OK {
			return 1
		}
	}
	return 0
}

func retry(ctx context.Context, o *options, name string, fn func() result) result {
	var last result
	for i := 0; i < o.attempts; i++ {
		if ctx.Err() != nil {
			break
		}
		last = fn()
		last.Name = name
		if last.OK {
			return last
		}
		if i+1 < o.attempts {
			select {
			case <-ctx.Done():
			case <-time.After(o.retryDelay):
			}
		}
	}
	return last
}

func checkHealth(ctx context.Context, o *options) result {
	start := time.Now()
	status, body, _, err := do(ctx, o, http.MethodGet, "/healthz", nil, "")
	r := result{Status: status, Elapsed: time.Since(start).Milliseconds()}
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	if status != http.StatusOK {
		r.Detail = snippet(body)
		return r
	}
	var h struct {
		Status  string `json:"status"`
		Version string `json:"version"`
		DB      bool   `json:"db"`
		Redis   bool   `json:"redis"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		r.Detail = "decode: " + err.Error()
		return r
	}
	if h.Status != "ok" || !h.DB || !h.Redis {
		r.Detail = fmt.Sprintf("status=%q db=%v redis=%v", h.Status, h.DB, h.Redis)
		return r
	}
	if o.expectVer != "" && h.Version != o.expectVer {
		r.Detail = fmt.Sprintf("version=%q want %q", h.Version, o.expectVer)
		return r
	}
	r.OK = true
	r.Detail = "version=" + h.Version
	return r
}

func checkJSON(ctx context.Context, o *options, method, path string, body []byte, want int) result {
	start := time.Now()
	status, raw, _, err := do(ctx, o, method, path, body, "application/json")
	r := result{Status: status, Elapsed: time.Since(start).Milliseconds()}
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	if status != want {
		r.Detail = snippet(raw)
		return r
	}
	if want == http.StatusOK && !json.Valid(raw) {
		r.Detail = "response is not valid JSON: " + snippet(raw)
		return r
	}
	r.OK = true
	return r
}

func checkCORS(ctx context.Context, o *options) result {
	start := time.Now()
	headers := map[string]string{
		"Origin":                         o.origin,
		"Access-Control-Request-Method":  http.MethodGet,
		"Access-Control-Request-Headers": "content-type",
	}
	status, body, hdr, err := doHeaders(ctx, o, http.MethodOptions, fmt.Sprintf("/v1/p/%s/facilities", o.tenant), headers)
	r := result{Status: status, Elapsed: time.Since(start).Milliseconds()}
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	if status != http.StatusNoContent && status != http.StatusOK {
		r.Detail = snippet(body)
		return r
	}
	allow := hdr.Get("Access-Control-Allow-Origin")
	if allow == "" {
		r.Detail = "missing Access-Control-Allow-Origin"
		return r
	}
	if allow != "*" && allow != o.origin {
		r.Detail = fmt.Sprintf("Access-Control-Allow-Origin=%q want %q", allow, o.origin)
		return r
	}
	if o.strictCORS && allow == "*" {
		r.Detail = "wildcard CORS is not allowed in production"
		return r
	}
	r.OK = true
	return r
}

func checkTenantHealth(ctx context.Context, o *options) result {
	start := time.Now()
	status, body, _, err := doHeaders(ctx, o, http.MethodGet, "/v1/admin/tenant-health", map[string]string{
		"Authorization": "Bearer " + o.adminKey,
	})
	r := result{Status: status, Elapsed: time.Since(start).Milliseconds()}
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	if status != http.StatusOK {
		r.Detail = snippet(body)
		return r
	}
	var payload struct {
		Quarantined map[string]json.RawMessage `json:"quarantined"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		r.Detail = "decode: " + err.Error()
		return r
	}
	if _, fenced := payload.Quarantined[o.tenant]; fenced {
		r.Detail = "tenant is quarantined: " + string(payload.Quarantined[o.tenant])
		return r
	}
	r.OK = true
	return r
}

func do(ctx context.Context, o *options, method, path string, body []byte, contentType string) (int, []byte, http.Header, error) {
	headers := map[string]string{}
	if contentType != "" {
		headers["Content-Type"] = contentType
	}
	return doHeadersWithBody(ctx, o, method, path, headers, body)
}

func doHeaders(ctx context.Context, o *options, method, path string, headers map[string]string) (int, []byte, http.Header, error) {
	return doHeadersWithBody(ctx, o, method, path, headers, nil)
}

func doHeadersWithBody(ctx context.Context, o *options, method, path string, headers map[string]string, body []byte) (int, []byte, http.Header, error) {
	var rdr io.Reader
	if body != nil {
		rdr = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, o.baseURL+path, rdr)
	if err != nil {
		return 0, nil, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if o.bypassSecret != "" {
		req.Header.Set("x-vercel-protection-bypass", o.bypassSecret)
	}
	client := &http.Client{Timeout: o.timeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, resp.Header, err
	}
	return resp.StatusCode, raw, resp.Header, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 240 {
		s = s[:240] + "..."
	}
	if s == "" {
		return "(empty body)"
	}
	return s
}
