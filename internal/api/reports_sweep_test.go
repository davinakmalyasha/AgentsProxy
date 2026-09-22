package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSweepOldReports(t *testing.T) {
	// Point the sweeper at a throwaway dir.
	old := reportsRoot
	reportsRoot = t.TempDir()
	defer func() { reportsRoot = old }()

	ten := filepath.Join(reportsRoot, "ten-a")
	if err := os.MkdirAll(ten, 0o755); err != nil {
		t.Fatal(err)
	}
	oldFile := filepath.Join(ten, "old.xlsx")
	newFile := filepath.Join(ten, "new.xlsx")
	junk := filepath.Join(reportsRoot, "ten-a", "notes.txt")
	if err := os.WriteFile(oldFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(junk, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// old.xlsx is 2h old; new.xlsx is fresh.
	oldT := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldFile, oldT, oldT); err != nil {
		t.Fatal(err)
	}

	sweepOldReports()

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Errorf("old.xlsx should be removed (exists=%v)", err == nil)
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Errorf("new.xlsx should be kept: %v", err)
	}
	// Non-.xlsx files are never touched by the sweep.
	if _, err := os.Stat(junk); err != nil {
		t.Errorf("notes.txt should be left alone: %v", err)
	}
}
