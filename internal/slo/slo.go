// Package slo derives server-observed latency SLOs from real production
// traffic instead of a synthetic tenant.
//
// Why: the public-read probe (cmd/bench) is a change-verification tool, not a
// statement about what staff, admins, or agent users actually experience.
// Those paths require credentials and spend LLM money, so probing them from CI
// is either impossible or dishonest. This package records every matched
// request locally, aggregates it into coarse histogram buckets, and flushes
// those buckets to Redis once per interval. An admin endpoint reads the last
// window back and reports p50/p95/p99 per route class.
//
// Honesty rules:
//   - Below minSamples in a window, a class reports "insufficient" instead of
//     a percentile computed from three requests.
//   - Provider-bound classes (chat, agent) report percentiles but never
//     "breach": their p95 is dominated by the LLM, not by this server.
//   - Percentiles are bucket-approximate by construction; the report says so.
//
// Cost: one Lua EVAL per class with traffic per flush interval (5 min). The
// script loops HINCRBY internally, so Upstash bills one command per class.
// At idle that is zero commands; at full traffic ~1.7k commands/day.
package slo

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Bucket bounds in milliseconds. Resolution is finer where server time lives
// (5-50 ms) and coarser where cold starts and provider calls live (1-10 s).
var boundsMs = [...]float64{
	5, 10, 15, 20, 25, 30, 40, 50, 75, 100, 150, 200,
	300, 400, 500, 750, 1000, 1500, 2000, 3000, 5000, 10000,
}

// Classes in stable report order.
var classes = []string{"public", "staff", "admin", "agent", "chat", "other"}

// Informational classes: their p95 is provider-bound, so a threshold breach
// there says nothing about this server. They still report percentiles.
var providerBound = map[string]bool{"agent": true, "chat": true}

// Server-observed regression ceilings (milliseconds). These are not
// client-observed SLO promises: the edge adds roughly 50-100 ms, so the
// client-observed targets in docs/PERF stay the published ones.
var targets = map[string]float64{
	"public": 300,
	"staff":  500,
	"admin":  400,
	"other":  500,
}

const (
	bucketSeconds   = 300 // 5-minute aggregation window
	flushInterval   = 5 * time.Minute
	redisTTLSeconds = 7200 // 2 h: enough for a 4-hour report window with slack
	minSamples      = 20
	defaultPrefix   = "slo"
)

type classState struct {
	counts [len(boundsMs) + 1]uint64 // last slot = overflow (>10 s)
	errors uint64
	total  uint64
}

type snapshot struct {
	at   time.Time
	data map[string]classState
}

var (
	mu        sync.Mutex
	rdb       *redis.Client
	prefix    = defaultPrefix
	state     = map[string]*classState{}
	lastFlush = time.Now()
)

// Configure installs the Redis client and key prefix, and resets local state.
// Called once at boot. prefix should not contain ':'.
func Configure(client *redis.Client, keyPrefix string) {
	mu.Lock()
	defer mu.Unlock()
	rdb = client
	if keyPrefix != "" {
		prefix = keyPrefix
	}
	state = map[string]*classState{}
	lastFlush = time.Now()
}

func bucketIndex(d time.Duration) int {
	ms := float64(d) / float64(time.Millisecond)
	if ms <= 0 {
		return 0
	}
	// Small linear scan: 22 bounds, once per request, no allocation.
	for i, b := range boundsMs {
		if ms <= b {
			return i
		}
	}
	return len(boundsMs) // overflow
}

// Observe records one request. status >= 500 counts as an error. It also
// triggers an asynchronous flush when the interval has elapsed. Safe for
// concurrent use; never blocks on Redis.
func Observe(class string, dur time.Duration, status int) {
	if class == "" {
		return
	}
	mu.Lock()
	cs := state[class]
	if cs == nil {
		cs = &classState{}
		state[class] = cs
	}
	cs.counts[bucketIndex(dur)]++
	cs.total++
	if status >= 500 {
		cs.errors++
	}
	var snap *snapshot
	if time.Since(lastFlush) >= flushInterval && rdb != nil {
		snap = swapLocked(time.Now())
		lastFlush = time.Now()
	}
	mu.Unlock()
	if snap != nil {
		// Detached and bounded: the client response is already written, and a
		// Redis stall must not hold the request goroutine. A lost window on
		// process freeze is acceptable for statistics; it is never user data.
		go flushSnapshot(snap)
	}
}

func swapLocked(at time.Time) *snapshot {
	if len(state) == 0 {
		return nil
	}
	data := make(map[string]classState, len(state))
	for k, v := range state {
		data[k] = *v
	}
	state = map[string]*classState{}
	return &snapshot{at: at, data: data}
}

// Flush writes any collected samples immediately. Used on shutdown; the
// periodic path calls it via Observe. Errors are logged, never returned:
// SLO statistics are best-effort.
func Flush() {
	mu.Lock()
	snap := swapLocked(time.Now())
	lastFlush = time.Now()
	client := rdb
	pfx := prefix
	mu.Unlock()
	if snap == nil || client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := writeSnapshot(ctx, client, pfx, snap); err != nil {
		slog.Warn("slo flush failed", "err", err)
	}
}

func flushSnapshot(snap *snapshot) {
	mu.Lock()
	client, pfx := rdb, prefix
	mu.Unlock()
	if client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := writeSnapshot(ctx, client, pfx, snap); err != nil {
		slog.Warn("slo flush failed", "err", err)
	}
}

func bucketKey(pfx, class string, bucketID int64) string {
	return pfx + ":" + class + ":" + strconv.FormatInt(bucketID, 10)
}

var flushScript = redis.NewScript(`
local errs = tonumber(ARGV[1])
if errs and errs > 0 then
  redis.call('HINCRBY', KEYS[1], 'e', errs)
end
for i = 2, #ARGV do
  local c = tonumber(ARGV[i])
  if c and c > 0 then
    redis.call('HINCRBY', KEYS[1], i - 2, c)
  end
end
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[#ARGV]))
return 1
`)

// writeSnapshot sends one EVAL per class with traffic. ARGV is
// [errors, bucket0..bucketN, ttlSeconds]; fields are numeric bucket indexes
// plus "e" for errors.
func writeSnapshot(ctx context.Context, client *redis.Client, pfx string, snap *snapshot) error {
	if len(snap.data) == 0 {
		return nil
	}
	bucketID := snap.at.Unix() / bucketSeconds
	var firstErr error
	for class, cs := range snap.data {
		if cs.total == 0 {
			continue
		}
		argv := make([]any, 0, len(cs.counts)+2)
		argv = append(argv, int64(cs.errors))
		for _, n := range cs.counts {
			argv = append(argv, int64(n))
		}
		argv = append(argv, int64(redisTTLSeconds))
		if err := flushScript.Run(ctx, client, []string{bucketKey(pfx, class, bucketID)}, argv...).Err(); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// ---- report ----

// ClassReport is one route class's aggregate over the report window.
type ClassReport struct {
	Class       string  `json:"class"`
	Requests    uint64  `json:"requests"`
	Errors      uint64  `json:"errors"`
	P50Ms       float64 `json:"p50_ms"`
	P95Ms       float64 `json:"p95_ms"`
	P99Ms       float64 `json:"p99_ms"`
	TargetP95Ms float64 `json:"target_p95_ms,omitempty"`
	Status      string  `json:"status"` // ok | insufficient | breach
	Approximate bool    `json:"approximate"`
}

// Report is the endpoint payload.
type Report struct {
	WindowMinutes int           `json:"window_minutes"`
	BucketSeconds int           `json:"bucket_seconds"`
	MinSamples    int           `json:"min_samples"`
	GeneratedAt   string        `json:"generated_at"`
	Classes       []ClassReport `json:"classes"`
	Notes         []string      `json:"notes,omitempty"`
}

var mergeScript = redis.NewScript(`
local out = {}
for k = 1, #KEYS do
  local h = redis.call('HGETALL', KEYS[k])
  for i = 1, #h, 2 do
    local f = h[i]
    out[f] = (out[f] or 0) + tonumber(h[i + 1])
  end
end
local flat = {}
for f, v in pairs(out) do
  flat[#flat + 1] = tostring(f)
  flat[#flat + 1] = tostring(v)
end
return flat
`)

// BuildReport aggregates the last windowMinutes of flushed buckets. It requires
// Configure to have been called with a non-nil client.
func BuildReport(ctx context.Context, windowMinutes int) (*Report, error) {
	if windowMinutes < 5 {
		windowMinutes = 5
	}
	if windowMinutes > 240 {
		windowMinutes = 240
	}
	mu.Lock()
	client, pfx := rdb, prefix
	mu.Unlock()
	if client == nil {
		return nil, errors.New("slo: redis not configured")
	}

	buckets := (windowMinutes + bucketSeconds/60 - 1) / (bucketSeconds / 60)
	nowBucket := time.Now().Unix() / bucketSeconds
	rep := &Report{
		WindowMinutes: windowMinutes,
		BucketSeconds: bucketSeconds,
		MinSamples:    minSamples,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Notes: []string{
			"Server-observed request time (middleware), excluding edge/network.",
			"Percentiles are histogram-approximate (5 ms-10 s buckets).",
			"Classes below min_samples report insufficient, never a guessed p95.",
			"chat/agent are provider-bound and never marked as breaches.",
		},
	}

	for _, class := range classes {
		keys := make([]string, 0, buckets)
		for i := int64(0); i < int64(buckets); i++ {
			keys = append(keys, bucketKey(pfx, class, nowBucket-i))
		}
		raw, err := mergeScript.Run(ctx, client, keys).Slice()
		if err != nil && !errors.Is(err, redis.Nil) {
			return nil, err
		}
		counts, errs := decodeMerged(raw)
		rep.Classes = append(rep.Classes, buildClassReport(class, counts, errs))
	}
	return rep, nil
}

func decodeMerged(raw []any) ([]uint64, uint64) {
	counts := make([]uint64, len(boundsMs)+1)
	var errs uint64
	for i := 0; i+1 < len(raw); i += 2 {
		field, ok := raw[i].(string)
		if !ok {
			continue
		}
		val, ok := parseUint(raw[i+1])
		if !ok {
			continue
		}
		if field == "e" {
			errs = val
			continue
		}
		idx := int(parseIntField(field))
		if idx >= 0 && idx < len(counts) {
			counts[idx] = val
		}
	}
	return counts, errs
}

func parseUint(v any) (uint64, bool) {
	switch t := v.(type) {
	case string:
		var n uint64
		ok := true
		if t == "" {
			return 0, false
		}
		for i := 0; i < len(t); i++ {
			c := t[i]
			if c < '0' || c > '9' {
				ok = false
				break
			}
			n = n*10 + uint64(c-'0')
		}
		return n, ok
	case int64:
		if t < 0 {
			return 0, false
		}
		return uint64(t), true
	default:
		return 0, false
	}
}

func parseIntField(s string) int64 {
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

func buildClassReport(class string, counts []uint64, errs uint64) ClassReport {
	var total uint64
	for _, n := range counts {
		total += n
	}
	cr := ClassReport{
		Class:       class,
		Requests:    total,
		Errors:      errs,
		Approximate: true,
		TargetP95Ms: targets[class],
	}
	if total == 0 {
		cr.Status = "insufficient"
		return cr
	}
	cr.P50Ms = percentileFromCounts(counts, 0.50, total)
	cr.P95Ms = percentileFromCounts(counts, 0.95, total)
	cr.P99Ms = percentileFromCounts(counts, 0.99, total)

	switch {
	case total < minSamples:
		cr.Status = "insufficient"
	case providerBound[class]:
		// Reported for visibility, never a breach: the LLM dominates.
		cr.Status = "ok"
	case cr.TargetP95Ms > 0 && cr.P95Ms > cr.TargetP95Ms:
		cr.Status = "breach"
	default:
		cr.Status = "ok"
	}
	return cr
}

// percentileFromCounts returns the upper bound of the bucket where the
// cumulative count first reaches the target rank. The overflow bucket reports
// its lower bound (the largest finite bound) so a p95 above 10 s does not
// masquerade as a precise number.
func percentileFromCounts(counts []uint64, p float64, total uint64) float64 {
	rank := uint64(math.Ceil(p * float64(total)))
	if rank < 1 {
		rank = 1
	}
	var cum uint64
	for i, n := range counts {
		cum += n
		if cum >= rank {
			if i < len(boundsMs) {
				return boundsMs[i]
			}
			return boundsMs[len(boundsMs)-1]
		}
	}
	return boundsMs[len(boundsMs)-1]
}

// EnsureConfigured installs the client only when nothing is configured yet.
// Handlers call it so a host that skipped main's boot wiring (tests, edge
// deployments) still reports instead of returning 503. It never resets a
// configured collector: Observe state must survive.
func EnsureConfigured(client *redis.Client, keyPrefix string) {
	if client == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if rdb != nil {
		return
	}
	rdb = client
	if keyPrefix != "" {
		prefix = keyPrefix
	}
	state = map[string]*classState{}
	lastFlush = time.Now()
}

// MaybeFlush starts a detached flush when the interval has elapsed. The
// middleware calls it at request start, so the goroutine has the whole
// request duration to finish before a serverless runtime freezes the
// instance — materially more durable than flushing only after the response.
// Non-blocking; a no-op when Redis is unconfigured or the interval is not due.
func MaybeFlush() {
	mu.Lock()
	var snap *snapshot
	if rdb != nil && time.Since(lastFlush) >= flushInterval {
		snap = swapLocked(time.Now())
		lastFlush = time.Now()
	}
	mu.Unlock()
	if snap != nil {
		go flushSnapshot(snap)
	}
}
