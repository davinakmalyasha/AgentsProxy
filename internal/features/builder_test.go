package features

import "testing"

func TestVerticalsToMount_NilAndEmptyMountNothing(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	if got := VerticalsToMount(nil); len(got) != 0 {
		t.Fatalf("nil live mounted %v, want none", got)
	}
	if got := VerticalsToMount([]string{}); len(got) != 0 {
		t.Fatalf("empty live mounted %v, want none", got)
	}
}

func TestVerticalsToMount_LiveOnly(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	got := VerticalsToMount([]string{"sports"})
	if len(got) != 1 || got[0] != "sports" {
		t.Fatalf("live=[sports] mounted %v", got)
	}
}

func TestVerticalsToMount_UnknownVerticalIgnored(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	got := VerticalsToMount([]string{"sports", "nope"})
	if len(got) != 1 || got[0] != "sports" {
		t.Fatalf("unknown vertical should be ignored, mounted %v", got)
	}
}

func TestVerticalsToMount_Deduplicates(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	got := VerticalsToMount([]string{"fnb", "fnb", "sports"})
	if len(got) != 2 || got[0] != "fnb" || got[1] != "sports" {
		t.Fatalf("duplicates not removed: %v", got)
	}
}
