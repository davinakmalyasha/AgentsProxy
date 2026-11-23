package middleware

import "testing"

func TestRedactPath(t *testing.T) {
	cases := map[string]string{
		"/v1/reports/0123456789abcdef.xlsx":                          "/v1/reports/[REDACTED]",
		"/v1/reports/0123456789abcdef":                               "/v1/reports/[REDACTED]",
		"/v1/reports/jobs/00000000-0000-0000-0000-000000000001":      "/v1/reports/jobs/[REDACTED]",
		"/v1/reports/jobs/00000000-0000-0000-0000-000000000001.xlsx": "/v1/reports/jobs/[REDACTED]",
		"/healthz":                 "/healthz",
		"/v1/p/centrum/facilities": "/v1/p/centrum/facilities",
	}
	for in, want := range cases {
		if got := RedactPath(in); got != want {
			t.Errorf("RedactPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFingerprint(t *testing.T) {
	if Fingerprint("token-a") == Fingerprint("token-b") {
		t.Fatal("distinct capability values collided")
	}
	if got := Fingerprint("token-a"); len(got) != 8 {
		t.Fatalf("Fingerprint length = %d, want 8", len(got))
	}
	if Fingerprint("token-a") == "token-a" {
		t.Fatal("Fingerprint returned the raw value")
	}
}
