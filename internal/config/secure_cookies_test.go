package config

import "testing"

func TestSecureCookies(t *testing.T) {
	t.Setenv("VERCEL", "")
	cases := map[string]bool{
		"development": false,
		"staging":     true,
		"production":  true,
		"":            true, // fail closed when a caller did not label the env
	}
	for env, want := range cases {
		c := &Config{Env: env}
		if got := c.SecureCookies(); got != want {
			t.Errorf("SecureCookies(%s) = %v, want %v", env, got, want)
		}
	}

	// A Vercel preview is HTTPS even when its ENV label is development.
	t.Setenv("VERCEL", "1")
	if c := (&Config{Env: "development"}); !c.SecureCookies() {
		t.Error("Vercel preview must set Secure cookies")
	}
}
