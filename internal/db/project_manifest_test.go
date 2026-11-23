package db

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestLoadProjectPlansRequiresManifest(t *testing.T) {
	fsys := fstest.MapFS{
		"projects/nofile/migrations/0001_init.sql": &fstest.MapFile{
			Data: []byte("SELECT 1;"),
		},
	}
	_, err := loadProjectPlans(fsys)
	if err == nil || !strings.Contains(err.Error(), "project.json is required") {
		t.Fatalf("missing manifest error = %v", err)
	}
}
