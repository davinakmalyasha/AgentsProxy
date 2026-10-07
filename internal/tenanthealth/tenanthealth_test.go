// Quarantine registry tests: one bad tenant fences alone, healthy
// tenants are unaffected, and the 503 body never collides with the
// lookup-infra message.
package tenanthealth

import (
	"errors"
	"strings"
	"testing"
)

func TestQuarantineCheckRelease(t *testing.T) {
	if q, _ := Check("ghost"); q {
		t.Fatal("unknown tenant quarantined")
	}
	Quarantine("sick", "bad folder")
	defer Release("sick")
	q, reason := Check("sick")
	if !q || reason != "bad folder" {
		t.Errorf("check = %v %q, want true + reason", q, reason)
	}
	if q, _ := Check("healthy"); q {
		t.Error("unrelated tenant fenced")
	}
	if !Release("sick") {
		t.Error("release reported false for fenced tenant")
	}
	if q, _ := Check("sick"); q {
		t.Error("tenant still fenced after release")
	}
	if Release("never") {
		t.Error("release reported true for never-fenced tenant")
	}
}

func TestQuarantineBodyIsDistinct(t *testing.T) {
	if ResponseBody == "tenant lookup unavailable" {
		t.Error("quarantine body collides with lookup-infra 503")
	}
	if !errors.Is(ErrQuarantined, ErrQuarantined) {
		t.Error("sentinel broken")
	}
	if strings.TrimSpace(ResponseBody) == "" {
		t.Error("empty quarantine body")
	}
}

func TestSnapshotListsFenced(t *testing.T) {
	Quarantine("a", "r1")
	defer Release("a")
	Quarantine("b", "r2")
	defer Release("b")
	snap := Snapshot()
	if len(snap) != 2 || snap["a"].Reason != "r1" || snap["b"].Reason != "r2" {
		t.Errorf("snapshot = %v", snap)
	}
	if snap["a"].Since.IsZero() {
		t.Error("missing fence timestamp")
	}
}
