package config

import (
	"errors"
	"os"
	"strings"
)

// LoadDatabase loads only the database-facing configuration used by the
// migration and provisioning CLIs. They do not need the LLM, admin, Sentry,
// or agency secrets, and forcing them to invent dummy values made it too easy
// to point a laptop at production.
func LoadDatabase() (*Config, error) {
	env := normalizeEnv(getenv("ENV", "development"))
	if err := validateEnv(env); err != nil {
		return nil, err
	}
	c := &Config{
		Env:              env,
		DatabaseURL:      strings.TrimSpace(os.Getenv("DATABASE_URL")),
		DatabaseAdminURL: strings.TrimSpace(os.Getenv("DATABASE_ADMIN_URL")),
		ExpectedDBRef:    strings.TrimSpace(os.Getenv("EXPECTED_DB_REF")),
	}
	if c.DatabaseAdminURL == "" {
		c.DatabaseAdminURL = c.DatabaseURL
	}
	if c.DatabaseURL == "" {
		c.DatabaseURL = c.DatabaseAdminURL
	}
	if c.DatabaseURL == "" {
		return nil, errors.New("DATABASE_URL or DATABASE_ADMIN_URL is required")
	}
	var err error
	if c.AllowRemoteDB, err = getenvBool("ALLOW_REMOTE_DB", false); err != nil {
		return nil, err
	}
	if err := validateDatabaseTargets(c); err != nil {
		return nil, err
	}
	return c, nil
}
