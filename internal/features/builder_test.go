package features

import "testing"

func TestProjectSchemaIsDerived(t *testing.T) {
	cases := map[string]string{
		"centrum":     "project_centrum",
		"warung-sari": "project_warung_sari",
		"a":           "project_a",
	}
	for slug, want := range cases {
		if got := ProjectSchema(slug); got != want {
			t.Errorf("ProjectSchema(%q) = %q, want %q", slug, got, want)
		}
	}
}
