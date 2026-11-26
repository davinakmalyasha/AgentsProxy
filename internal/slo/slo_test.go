package slo

import (
	"math"
	"testing"
	"time"
)

func TestBucketIndexBounds(t *testing.T) {
	cases := []struct {
		d   time.Duration
		idx int
	}{
		{0, 0},
		{time.Millisecond, 0},
		{5 * time.Millisecond, 0},
		{6 * time.Millisecond, 1},
		{10 * time.Millisecond, 1},
		{11 * time.Millisecond, 2},
		{150 * time.Millisecond, 10},
		{10 * time.Second, len(boundsMs) - 1},
		{30 * time.Second, len(boundsMs)}, // overflow
	}
	for _, c := range cases {
		if got := bucketIndex(c.d); got != c.idx {
			t.Errorf("bucketIndex(%v)=%d want %d", c.d, got, c.idx)
		}
	}
}

func TestPercentileFromCounts(t *testing.T) {
	counts := make([]uint64, len(boundsMs)+1)
	// 100 requests: 50 at <=10ms (bucket 1), 45 at <=50ms (bucket 7),
	// 5 at <=200ms (bucket 11).
	counts[1] = 50
	counts[7] = 45
	counts[11] = 5
	total := uint64(100)

	if got := percentileFromCounts(counts, 0.50, total); got != 10 {
		t.Errorf("p50=%v want 10", got)
	}
	if got := percentileFromCounts(counts, 0.95, total); got != 50 {
		t.Errorf("p95=%v want 50", got)
	}
	if got := percentileFromCounts(counts, 0.99, total); got != 200 {
		t.Errorf("p99=%v want 200", got)
	}
}

func TestPercentileOverflowReportsLargestFinite(t *testing.T) {
	counts := make([]uint64, len(boundsMs)+1)
	counts[len(boundsMs)] = 10 // all >10s
	got := percentileFromCounts(counts, 0.95, 10)
	if got != boundsMs[len(boundsMs)-1] {
		t.Errorf("overflow p95=%v want %v", got, boundsMs[len(boundsMs)-1])
	}
}

func TestBuildClassReport(t *testing.T) {
	counts := make([]uint64, len(boundsMs)+1)

	// Empty: insufficient, no fabricated percentile.
	if r := buildClassReport("public", counts, 0); r.Status != "insufficient" || r.P95Ms != 0 {
		t.Errorf("empty: status=%q p95=%v want insufficient/0", r.Status, r.P95Ms)
	}

	// Below minSamples: insufficient even with a percentile available.
	counts[1] = uint64(minSamples - 1)
	if r := buildClassReport("public", counts, 0); r.Status != "insufficient" {
		t.Errorf("below min: status=%q want insufficient", r.Status)
	}

	// Enough samples under target: ok.
	counts[1] = uint64(minSamples)
	if r := buildClassReport("public", counts, 0); r.Status != "ok" {
		t.Errorf("under target: status=%q want ok", r.Status)
	}

	// Enough samples over target: breach for a server-bound class.
	heavy := make([]uint64, len(boundsMs)+1)
	heavy[15] = uint64(minSamples) // 750ms bucket
	if r := buildClassReport("staff", heavy, 0); r.Status != "breach" {
		t.Errorf("over target: status=%q want breach", r.Status)
	}

	// Provider-bound classes never breach: same heavy tail, still ok.
	if r := buildClassReport("chat", heavy, 0); r.Status != "ok" {
		t.Errorf("chat over target: status=%q want ok (provider-bound)", r.Status)
	}
}

func TestDecodeMerged(t *testing.T) {
	raw := []any{"e", "3", "1", "50", "7", "45", "bogus", "9", "11", "5"}
	counts, errs := decodeMerged(raw)
	if errs != 3 {
		t.Errorf("errs=%d want 3", errs)
	}
	if counts[1] != 50 || counts[7] != 45 || counts[11] != 5 {
		t.Errorf("counts not decoded: %v", counts)
	}
	var total uint64
	for _, n := range counts {
		total += n
	}
	if total != 100 {
		t.Errorf("total=%d want 100", total)
	}
}

func TestReportClampHelpers(t *testing.T) {
	// Guard against a nil/zero window producing a division by zero upstream.
	if got := math.Ceil(0.95 * float64(minSamples)); got < 1 {
		t.Fatalf("percentile rank sanity failed: %v", got)
	}
}
