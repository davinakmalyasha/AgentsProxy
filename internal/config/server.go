package config

import (
	"fmt"
	"os"
	"strings"
)

// ValidateServerSecrets is the server-only hardening pass. cmd/migrate and
// cmd/provision intentionally use a dummy ADMIN_API_KEY/LLM_API_KEY; they
// must not be forced to invent real secrets. cmd/server calls this before it
// opens a pool or starts the router.
func (c *Config) ValidateServerSecrets() error {
	key := strings.TrimSpace(c.AdminAPIKey)
	if len(key) < 32 {
		return fmt.Errorf("ADMIN_API_KEY must be at least 32 characters; got %d", len(key))
	}
	upper := strings.ToUpper(key)
	for _, placeholder := range []string{"REPLACE_ME", "CHANGE_ME", "MIGRATE-UNUSED", "TEST-ADMIN"} {
		if strings.Contains(upper, placeholder) {
			return fmt.Errorf("ADMIN_API_KEY still contains the placeholder %q", placeholder)
		}
	}
	if c.AgencyJWTSecret != "" && len(c.AgencyJWTSecret) < 32 {
		return fmt.Errorf("JWT_SECRET must be at least 32 characters")
	}

	switch strings.ToLower(strings.TrimSpace(os.Getenv("VERCEL_ENV"))) {
	case "production", "preview":
		if c.BackgroundTickers {
			return fmt.Errorf("BACKGROUND_TICKERS must be false on Vercel; the process does not live between requests")
		}
		if !c.DBSimpleProtocol {
			return fmt.Errorf("DB_USE_SIMPLE_PROTOCOL must be true on Vercel's Supavisor 6543 pooler")
		}
		if c.DBPoolMin != 0 {
			return fmt.Errorf("DB_POOL_MIN must be 0 on Vercel; got %d", c.DBPoolMin)
		}
		if c.DBPoolMax > 5 {
			return fmt.Errorf("DB_POOL_MAX must be <= 5 on Vercel; got %d", c.DBPoolMax)
		}
	}
	return nil
}
