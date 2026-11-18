package auth

import (
	"context"
	"testing"

	platformauth "github.com/davinakmalyasha/agents-proxy/internal/agency/auth"
)

type stubNotifier struct{ configured bool }

func (s stubNotifier) SendEmail(context.Context, string, string, string) error { return nil }
func (s stubNotifier) ResendConfigured() bool                                  { return s.configured }

func TestNormalizeEmail(t *testing.T) {
	if got := NormalizeEmail("  Admin@DFDagency.COM "); got != "admin@dfdagency.com" {
		t.Fatalf("got %q", got)
	}
}

func TestPasswordPolicy(t *testing.T) {
	if !ValidPassword("superadmin123") {
		t.Fatal("expected valid password")
	}
	for _, bad := range []string{"short1", "12345678", "password", "abc", ""} {
		if ValidPassword(bad) {
			t.Fatalf("expected %q to be rejected", bad)
		}
	}
}

func TestSignupAvailable(t *testing.T) {
	cases := []struct {
		name     string
		env      string
		enabled  bool
		notifier stubNotifier
		want     bool
	}{
		{"prod ready", "production", true, stubNotifier{true}, true},
		{"prod no email", "production", true, stubNotifier{false}, false},
		{"prod switch off", "production", false, stubNotifier{true}, false},
		{"dev no email logs", "development", true, stubNotifier{false}, true},
		{"dev switch off", "development", false, stubNotifier{true}, false},
	}
	for _, tc := range cases {
		h := &Handler{env: tc.env, signupEnabled: tc.enabled, notifier: tc.notifier}
		if got := h.signupAvailable(); got != tc.want {
			t.Errorf("%s: signupAvailable() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestOTPHash(t *testing.T) {
	a, b := HashOTPCode("123456"), HashOTPCode("123456")
	if a != b || len(a) != 64 {
		t.Fatalf("hash not deterministic hex64: %q", a)
	}
	if HashOTPCode("123456") == HashOTPCode("654321") {
		t.Fatal("different codes must hash differently")
	}
}

func TestGenerateOTPCode(t *testing.T) {
	for i := 0; i < 20; i++ {
		c, err := generateOTPCode()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if len(c) != 6 {
			t.Fatalf("want 6 digits, got %q", c)
		}
		for _, r := range c {
			if r < '0' || r > '9' {
				t.Fatalf("non-digit in %q", c)
			}
		}
	}
}

func TestMaskEmail(t *testing.T) {
	if got := maskEmail("admin@dfdagency.com"); got != "ad***@dfdagency.com" {
		t.Fatalf("got %q", got)
	}
	if got := maskEmail("a@x.id"); got != "***" {
		t.Fatalf("got %q", got)
	}
}

func TestSecureCompare(t *testing.T) {
	hash, err := platformauth.HashPassword("demo1234")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !SecureCompare("demo1234", hash) {
		t.Fatal("expected match")
	}
	if SecureCompare("wrong", hash) {
		t.Fatal("expected mismatch")
	}
	if SecureCompare("anything", "") {
		t.Fatal("expected empty hash to fail")
	}
}
