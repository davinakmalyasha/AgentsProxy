package slo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
)

// Integration: real Redis, isolated prefix, exact round trip through
// Observe -> Flush -> BuildReport. Skipped without TEST_REDIS_URL.
func TestFlushAndReport(t *testing.T) {
	rurl := os.Getenv("TEST_REDIS_URL")
	if rurl == "" {
		t.Skip("TEST_REDIS_URL not set — skipping Redis integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rdb, err := redisx.NewClient(ctx, rurl)
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()

	b := make([]byte, 4)
	_, _ = rand.Read(b)
	prefix := "slottest:" + hex.EncodeToString(b)
	t.Cleanup(func() {
		keys, _ := rdb.Keys(context.Background(), prefix+"*").Result()
		if len(keys) > 0 {
			_ = rdb.Del(context.Background(), keys...).Err()
		}
	})
	Configure(rdb, prefix)
	t.Cleanup(func() { Configure(nil, "slo") })

	// 30 public requests: 20 at 10ms, 10 at 400ms. 2 server errors.
	for i := 0; i < 20; i++ {
		Observe("public", 10*time.Millisecond, 200)
	}
	for i := 0; i < 10; i++ {
		Observe("public", 400*time.Millisecond, 200)
	}
	Observe("public", 5*time.Millisecond, 500)
	Observe("public", 5*time.Millisecond, 503)
	// Sparse class: must report insufficient, not a guessed percentile.
	Observe("admin", 20*time.Millisecond, 200)

	Flush()

	rep, err := BuildReport(ctx, 15)
	if err != nil {
		t.Fatal(err)
	}
	if rep.WindowMinutes != 15 || rep.MinSamples != minSamples {
		t.Errorf("report meta: %+v", rep)
	}

	byClass := map[string]ClassReport{}
	for _, c := range rep.Classes {
		byClass[c.Class] = c
	}

	pub := byClass["public"]
	if pub.Requests != 32 {
		t.Errorf("public requests=%d want 32", pub.Requests)
	}
	if pub.Errors != 2 {
		t.Errorf("public errors=%d want 2", pub.Errors)
	}
	if pub.P95Ms != 400 {
		t.Errorf("public p95=%v want 400 (bucket bound)", pub.P95Ms)
	}
	if pub.Status != "breach" {
		t.Errorf("public status=%q want breach (400ms > 300ms target)", pub.Status)
	}
	if !pub.Approximate {
		t.Error("public report must be marked approximate")
	}

	adm := byClass["admin"]
	if adm.Requests != 1 || adm.Status != "insufficient" {
		t.Errorf("admin=%+v want 1 request / insufficient", adm)
	}

	if got := byClass["agent"]; got.Status != "insufficient" || got.Requests != 0 {
		t.Errorf("agent=%+v want empty / insufficient", got)
	}
}

// TestObserveNeverBlocksWithoutRedis: collection must work in tests and in a
// Redisless local run; only flushing is skipped.
func TestObserveNeverBlocksWithoutRedis(t *testing.T) {
	Configure(nil, "slo")
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			Observe("staff", time.Duration(i)*time.Millisecond, 200)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Observe blocked without Redis")
	}
	Configure(nil, "slo")
}
