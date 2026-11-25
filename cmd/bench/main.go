// Command bench is the HTTP load harness for the exit-gate performance
// baseline. It intentionally uses the same endpoints and identity headers as
// production clients, so a regression in the route/feature/RLS chain shows up
// as latency or errors rather than being hidden by an in-process shortcut.
//
// Usage:
//
//	go run ./cmd/bench -base-url http://127.0.0.1:8080 \
//	  -client-token bench-client-token -staff-token bench-staff-token \
//	  -admin-key bench-admin -duration 30s
//
// It emits per-scenario p50/p90/p95/p99, throughput, status distribution, and
// errors to stdout (or JSON/Markdown files). Closed-loop concurrency is used
// on purpose: the baseline is about the server's behavior under a fixed
// number of in-flight requests, not a synthetic arrival process that can hide
// queueing.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type options struct {
	baseURL    string
	tenant     string
	clientTok  string
	staffTok   string
	staffEmail string
	staffPass  string
	adminKey   string
	conc       int
	warmup     time.Duration
	duration   time.Duration
	scenarios  string
	jsonPath   string
	mdPath     string
	timeout    time.Duration
	rps        float64
	cacheBust  bool
}

type sample struct {
	dur      time.Duration
	status   int
	err      bool
	serverMs float64
	instance string
	cold     bool
	cache    string
}

var cacheBustSeq atomic.Uint64

type instanceTracker struct {
	mu   sync.Mutex
	seen map[string]bool
}

func newInstanceTracker() *instanceTracker {
	return &instanceTracker{seen: map[string]bool{}}
}

// observe reports whether this is the first request seen for the instance.
// Empty IDs (protection pages, older servers) are neither cold nor warm.
func (t *instanceTracker) observe(id string) bool {
	if id == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	cold := !t.seen[id]
	t.seen[id] = true
	return cold
}

func (t *instanceTracker) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.seen)
}

type stats struct {
	Name         string      `json:"name"`
	Requests     int         `json:"requests"`
	Errors       int         `json:"errors"`
	RPS          float64     `json:"rps"`
	P50Ms        float64     `json:"p50_ms"`
	P90Ms        float64     `json:"p90_ms"`
	P95Ms        float64     `json:"p95_ms"`
	P99Ms        float64     `json:"p99_ms"`
	MinMs        float64     `json:"min_ms"`
	MaxMs        float64     `json:"max_ms"`
	ServerP50Ms  float64     `json:"server_p50_ms,omitempty"`
	ServerP95Ms  float64     `json:"server_p95_ms,omitempty"`
	ServerP99Ms  float64     `json:"server_p99_ms,omitempty"`
	Instances    int         `json:"instances,omitempty"`
	ColdRequests int         `json:"cold_requests,omitempty"`
	ColdP95Ms    float64     `json:"cold_p95_ms,omitempty"`
	WarmRequests int         `json:"warm_requests,omitempty"`
	WarmP95Ms    float64     `json:"warm_p95_ms,omitempty"`
	CacheHits    int         `json:"cache_hits,omitempty"`
	OriginReqs   int         `json:"origin_requests,omitempty"`
	OriginP95Ms  float64     `json:"origin_p95_ms,omitempty"`
	StatusCodes  map[int]int `json:"status_codes"`
	ElapsedMS    int64       `json:"elapsed_ms"`
	Notes        []string    `json:"notes,omitempty"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	var o options
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.baseURL, "base-url", "http://127.0.0.1:8080", "server base URL")
	fs.StringVar(&o.tenant, "tenant", "centrum", "project tenant")
	fs.StringVar(&o.clientTok, "client-token", "bench-client-token", "client token")
	fs.StringVar(&o.staffTok, "staff-token", "bench-staff-token", "staff session token")
	fs.StringVar(&o.staffEmail, "staff-email", "bench-owner@example.test", "staff login email")
	fs.StringVar(&o.staffPass, "staff-password", "bench-password", "staff login password")
	fs.StringVar(&o.adminKey, "admin-key", "", "admin API key (optional; admin scenarios skipped without it)")
	fs.IntVar(&o.conc, "concurrency", 16, "in-flight requests per scenario")
	fs.DurationVar(&o.warmup, "warmup", 10*time.Second, "warmup duration per scenario")
	fs.DurationVar(&o.duration, "duration", 30*time.Second, "measurement duration per scenario")
	fs.StringVar(&o.scenarios, "scenarios", "all", "comma-separated scenario names, or all")
	fs.StringVar(&o.jsonPath, "json", "", "write JSON results to this path")
	fs.StringVar(&o.mdPath, "markdown", "", "write a Markdown table to this path")
	fs.DurationVar(&o.timeout, "timeout", 60*time.Second, "per-request timeout")
	fs.Float64Var(&o.rps, "rps", 0, "total target requests/second across workers (0 = closed-loop saturation)")
	fs.BoolVar(&o.cacheBust, "cache-bust", false, "append a unique query parameter to GETs so CDN cache hits are avoided (origin measurement)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	o.baseURL = strings.TrimRight(strings.TrimSpace(o.baseURL), "/")
	if o.baseURL == "" {
		fmt.Fprintln(stderr, "bench: -base-url is required")
		return 2
	}
	if o.conc < 1 {
		o.conc = 1
	}

	client := &http.Client{
		Timeout: o.timeout,
		Transport: &http.Transport{
			MaxIdleConns:        128,
			MaxIdleConnsPerHost: 128,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	loginCtx, loginCancel := context.WithTimeout(context.Background(), time.Minute)
	defer loginCancel()

	staffToken := strings.TrimSpace(o.staffTok)
	all := buildScenarios(&o, staffToken, func() string {
		return fmt.Sprintf("bench-%d", time.Now().UnixNano())
	})
	selected := selectScenarios(&o, all)
	if len(selected) == 0 {
		fmt.Fprintln(stderr, "bench: no scenarios selected")
		return 2
	}
	needsStaff := false
	for _, sc := range selected {
		if sc.name == "records-list" || sc.name == "records-create-delete" {
			needsStaff = true
		}
	}
	if needsStaff && staffToken == "" {
		tok, err := login(loginCtx, client, &o)
		if err != nil {
			fmt.Fprintln(stderr, "bench: staff login:", err)
			return 1
		}
		staffToken = tok
		all = buildScenarios(&o, staffToken, func() string {
			return fmt.Sprintf("bench-%d", time.Now().UnixNano())
		})
		selected = selectScenarios(&o, all)
	}

	// One budget per selected scenario; a fixed multiplier silently cut off
	// long multi-scenario baselines.
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(len(selected))*(o.duration+o.warmup)+2*time.Minute)
	defer cancel()

	var results []stats
	tracker := newInstanceTracker()
	for _, sc := range selected {
		if o.warmup > 0 {
			warm(ctx, client, sc, &o, tracker)
		}
		results = append(results, measure(ctx, client, sc, &o, tracker))
	}

	printTable(stdout, results)
	if o.jsonPath != "" {
		if err := writeJSON(o.jsonPath, results); err != nil {
			fmt.Fprintln(stderr, "bench: write json:", err)
			return 1
		}
	}
	if o.mdPath != "" {
		if err := writeMarkdown(o.mdPath, results); err != nil {
			fmt.Fprintln(stderr, "bench: write markdown:", err)
			return 1
		}
	}
	for _, r := range results {
		if r.Errors > 0 {
			return 1
		}
	}
	return 0
}

type scenario struct {
	name       string
	method     string
	path       func() string
	headers    map[string]string
	body       func() []byte
	want       map[int]bool
	createPath string // if set, POST then DELETE /<id>
}

func buildScenarios(o *options, staffToken string, uniq func() string) []scenario {
	public := "/v1/p/" + o.tenant
	staff := map[string]string{"X-Session-Token": staffToken}
	jsonH := map[string]string{"Content-Type": "application/json"}
	admin := map[string]string{"Authorization": "Bearer " + o.adminKey}

	out := []scenario{
		{name: "health", method: http.MethodGet, path: func() string { return "/healthz" }, want: ok(200)},
		{name: "public-facilities", method: http.MethodGet, path: func() string { return public + "/facilities" }, want: ok(200)},
		{name: "public-availability", method: http.MethodGet, path: func() string {
			return public + "/availability?date=" + time.Now().UTC().Format("2006-01-02")
		}, want: ok(200)},
		{name: "public-pricing", method: http.MethodGet, path: func() string { return public + "/pricing" }, want: ok(200)},
		{name: "records-list", method: http.MethodGet, path: func() string { return public + "/records" }, headers: staff, want: ok(200)},
		{
			name: "records-create-delete", method: http.MethodPost, path: func() string { return public + "/records" },
			headers: merge(staff, jsonH), body: func() []byte {
				return []byte(fmt.Sprintf(`{"kind":"item","title":%q,"payload":{}}`, uniq()))
			},
			want: ok(201), createPath: public + "/records",
		},
		{
			name: "staff-login", method: http.MethodPost, path: func() string { return "/v1/auth/login" },
			headers: jsonH, body: func() []byte {
				return []byte(fmt.Sprintf(`{"tenant_id":%q,"email":%q,"password":%q}`, o.tenant, o.staffEmail, o.staffPass))
			}, want: ok(200),
		},
		{
			name: "agent-client-chat", method: http.MethodPost, path: func() string { return "/v1/agent/client-chat" },
			headers: jsonH, body: func() []byte {
				return []byte(fmt.Sprintf(`{"tenant_id":%q,"message":"ping"}`, o.tenant))
			}, want: ok(200),
		},
		{
			name: "chat", method: http.MethodPost, path: func() string { return "/v1/chat" },
			headers: jsonH, body: func() []byte {
				return []byte(fmt.Sprintf(`{"client_token":%q,"messages":[{"role":"user","content":"ping"}]}`, o.clientTok))
			}, want: ok(200),
		},
	}
	if o.adminKey != "" {
		out = append(out,
			scenario{name: "admin-clients", method: http.MethodGet, path: func() string { return "/v1/admin/clients" }, headers: admin, want: ok(200)},
			scenario{name: "admin-tenant-health", method: http.MethodGet, path: func() string { return "/v1/admin/tenant-health" }, headers: admin, want: ok(200)},
		)
	}
	return out
}

func ok(codes ...int) map[int]bool {
	m := map[int]bool{}
	for _, c := range codes {
		m[c] = true
	}
	return m
}

func merge(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func selectScenarios(o *options, all []scenario) []scenario {
	if strings.TrimSpace(o.scenarios) == "" || o.scenarios == "all" {
		return all
	}
	want := map[string]bool{}
	for _, name := range strings.Split(o.scenarios, ",") {
		want[strings.TrimSpace(name)] = true
	}
	out := make([]scenario, 0, len(want))
	for _, sc := range all {
		if want[sc.name] {
			out = append(out, sc)
		}
	}
	return out
}

func warm(ctx context.Context, client *http.Client, sc scenario, o *options, tracker *instanceTracker) {
	wctx, cancel := context.WithTimeout(ctx, o.warmup)
	defer cancel()
	var pacing time.Duration
	if o.rps > 0 {
		pacing = time.Duration(float64(time.Second) * float64(o.conc) / o.rps)
	}
	var wg sync.WaitGroup
	for i := 0; i < o.conc; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if pacing > 0 && i > 0 {
				select {
				case <-wctx.Done():
					return
				case <-time.After(time.Duration(int64(pacing) * int64(i) / int64(o.conc))):
				}
			}
			for wctx.Err() == nil {
				started := time.Now()
				_ = do(wctx, client, sc, o, tracker)
				if pacing > 0 {
					if wait := pacing - time.Since(started); wait > 0 {
						select {
						case <-wctx.Done():
							return
						case <-time.After(wait):
						}
					}
				}
			}
		}(i)
	}
	wg.Wait()
}

func measure(ctx context.Context, client *http.Client, sc scenario, o *options, tracker *instanceTracker) stats {
	mctx, cancel := context.WithTimeout(ctx, o.duration)
	defer cancel()
	start := time.Now()
	var (
		mu      sync.Mutex
		samples []sample
		wg      sync.WaitGroup
	)
	var pacing time.Duration
	if o.rps > 0 {
		pacing = time.Duration(float64(time.Second) * float64(o.conc) / o.rps)
	}
	for i := 0; i < o.conc; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if pacing > 0 && i > 0 {
				select {
				case <-mctx.Done():
					return
				case <-time.After(time.Duration(int64(pacing) * int64(i) / int64(o.conc))):
				}
			}
			for mctx.Err() == nil {
				started := time.Now()
				s := do(mctx, client, sc, o, tracker)
				// Requests interrupted by the measurement deadline are not
				// server errors; drop them instead of poisoning the tail.
				if mctx.Err() != nil {
					break
				}
				mu.Lock()
				samples = append(samples, s)
				mu.Unlock()
				if pacing > 0 {
					if wait := pacing - time.Since(started); wait > 0 {
						select {
						case <-mctx.Done():
							return
						case <-time.After(wait):
						}
					}
				}
			}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	st := stats{Name: sc.name, StatusCodes: map[int]int{}, ElapsedMS: elapsed.Milliseconds()}
	durs := make([]time.Duration, 0, len(samples))
	serverDurs := make([]time.Duration, 0, len(samples))
	coldDurs := make([]time.Duration, 0, len(samples))
	warmDurs := make([]time.Duration, 0, len(samples))
	originDurs := make([]time.Duration, 0, len(samples))
	for _, s := range samples {
		st.Requests++
		st.StatusCodes[s.status]++
		if s.err || !sc.want[s.status] {
			st.Errors++
			continue
		}
		durs = append(durs, s.dur)
		if s.cache == "hit" {
			st.CacheHits++
			continue
		}
		st.OriginReqs++
		originDurs = append(originDurs, s.dur)
		if s.serverMs > 0 {
			serverDurs = append(serverDurs, time.Duration(s.serverMs*float64(time.Millisecond)))
		}
		if s.instance != "" {
			if s.cold {
				st.ColdRequests++
				coldDurs = append(coldDurs, s.dur)
			} else {
				st.WarmRequests++
				warmDurs = append(warmDurs, s.dur)
			}
		}
	}
	if elapsed > 0 {
		st.RPS = float64(st.Requests) / elapsed.Seconds()
	}
	st.Instances = tracker.count()
	if len(durs) > 0 {
		sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
		st.P50Ms = ms(percentile(durs, 0.50))
		st.P90Ms = ms(percentile(durs, 0.90))
		st.P95Ms = ms(percentile(durs, 0.95))
		st.P99Ms = ms(percentile(durs, 0.99))
		st.MinMs = ms(durs[0])
		st.MaxMs = ms(durs[len(durs)-1])
	}
	if len(serverDurs) > 0 {
		sort.Slice(serverDurs, func(i, j int) bool { return serverDurs[i] < serverDurs[j] })
		st.ServerP50Ms = ms(percentile(serverDurs, 0.50))
		st.ServerP95Ms = ms(percentile(serverDurs, 0.95))
		st.ServerP99Ms = ms(percentile(serverDurs, 0.99))
	}
	if len(coldDurs) > 0 {
		sort.Slice(coldDurs, func(i, j int) bool { return coldDurs[i] < coldDurs[j] })
		st.ColdP95Ms = ms(percentile(coldDurs, 0.95))
	}
	if len(warmDurs) > 0 {
		sort.Slice(warmDurs, func(i, j int) bool { return warmDurs[i] < warmDurs[j] })
		st.WarmP95Ms = ms(percentile(warmDurs, 0.95))
	}
	if len(originDurs) > 0 {
		sort.Slice(originDurs, func(i, j int) bool { return originDurs[i] < originDurs[j] })
		st.OriginP95Ms = ms(percentile(originDurs, 0.95))
	}
	return st
}

func do(ctx context.Context, client *http.Client, sc scenario, o *options, tracker *instanceTracker) sample {
	start := time.Now()
	body := []byte(nil)
	if sc.body != nil {
		body = sc.body()
	}
	path := sc.path()
	if o.cacheBust && sc.method == http.MethodGet {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		path += sep + "__bench_cb=" + strconv.FormatUint(cacheBustSeq.Add(1), 10)
	}
	req, err := http.NewRequestWithContext(ctx, sc.method, o.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return sample{dur: time.Since(start), err: true}
	}
	for k, v := range sc.headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return sample{dur: time.Since(start), err: true}
	}
	instance := resp.Header.Get("X-Instance-Id")
	serverMs := parseServerTiming(resp.Header.Get("Server-Timing"))
	cacheState := strings.ToLower(strings.TrimSpace(resp.Header.Get("X-Vercel-Cache")))
	cold := false
	if cacheState != "hit" {
		cold = tracker.observe(instance)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if sc.createPath != "" && resp.StatusCode == http.StatusCreated {
		var created struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &created) == nil && created.ID != "" {
			dr, err := http.NewRequestWithContext(ctx, http.MethodDelete, o.baseURL+sc.createPath+"/"+created.ID, nil)
			if err == nil {
				for k, v := range sc.headers {
					dr.Header.Set(k, v)
				}
				dresp, derr := client.Do(dr)
				if derr != nil {
					return sample{dur: time.Since(start), status: resp.StatusCode, err: true, serverMs: serverMs, instance: instance, cold: cold, cache: cacheState}
				}
				_, _ = io.Copy(io.Discard, io.LimitReader(dresp.Body, 1<<20))
				dresp.Body.Close()
				if dresp.StatusCode != http.StatusOK && dresp.StatusCode != http.StatusNoContent {
					return sample{dur: time.Since(start), status: resp.StatusCode, err: true, serverMs: serverMs, instance: instance, cold: cold, cache: cacheState}
				}
			}
		}
	}
	return sample{dur: time.Since(start), status: resp.StatusCode, serverMs: serverMs, instance: instance, cold: cold, cache: cacheState}
}

func parseServerTiming(header string) float64 {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "app;dur=") {
			continue
		}
		var ms float64
		if _, err := fmt.Sscanf(strings.TrimPrefix(part, "app;dur="), "%f", &ms); err == nil {
			return ms
		}
	}
	return 0
}

func login(ctx context.Context, client *http.Client, o *options) (string, error) {
	body := fmt.Sprintf(`{"tenant_id":%q,"email":%q,"password":%q}`, o.tenant, o.staffEmail, o.staffPass)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/v1/auth/login", strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var payload struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", err
	}
	if payload.Token == "" {
		return "", fmt.Errorf("login returned no token")
	}
	return payload.Token, nil
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func ms(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

func printTable(w io.Writer, results []stats) {
	fmt.Fprintf(w, "%-26s %8s %8s %8s %8s %8s %8s %8s %6s %8s %8s\n",
		"scenario", "req", "err", "rps", "p50ms", "p95ms", "p99ms", "srv95", "inst", "cold95", "warm95")
	for _, r := range results {
		fmt.Fprintf(w, "%-26s %8d %8d %8.1f %8.2f %8.2f %8.2f %8.2f %6d %8.2f %8.2f\n",
			r.Name, r.Requests, r.Errors, r.RPS, r.P50Ms, r.P95Ms, r.P99Ms,
			r.ServerP95Ms, r.Instances, r.ColdP95Ms, r.WarmP95Ms)
	}
}

func writeJSON(path string, results []stats) error {
	b, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func writeMarkdown(path string, results []stats) error {
	var b strings.Builder
	b.WriteString("| scenario | requests | errors | rps | p50 ms | p90 ms | p95 ms | p99 ms | server p95 | instances | cold p95 | warm p95 |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, r := range results {
		fmt.Fprintf(&b, "| %s | %d | %d | %.1f | %.2f | %.2f | %.2f | %.2f | %.2f | %d | %.2f | %.2f |\n",
			r.Name, r.Requests, r.Errors, r.RPS, r.P50Ms, r.P90Ms, r.P95Ms, r.P99Ms,
			r.ServerP95Ms, r.Instances, r.ColdP95Ms, r.WarmP95Ms)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
