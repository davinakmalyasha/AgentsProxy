package config

import (
	"strings"
	"testing"
)

func TestLoad_MissingRequired(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("ADMIN_API_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when required vars missing")
	}
}

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u@localhost/d")
	t.Setenv("LLM_API_KEY", "sk-test")
	t.Setenv("ADMIN_API_KEY", "admin-secret")
	t.Setenv("PORT", "")
	t.Setenv("REDIS_URL", "")
	t.Setenv("MAX_TOKENS_PER_REQUEST", "")
	t.Setenv("RATE_LIMIT_PER_MINUTE", "")
	t.Setenv("ALLOW_ORIGINS", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != "8080" {
		t.Errorf("Port default: %s", c.Port)
	}
	if c.RedisURL != "localhost:6379" {
		t.Errorf("RedisURL default: %s", c.RedisURL)
	}
	if c.MaxTokensPerReq != 2048 {
		t.Errorf("MaxTokens default: %d", c.MaxTokensPerReq)
	}
	if c.RateLimitPerMinute != 60 {
		t.Errorf("RateLimit default: %d", c.RateLimitPerMinute)
	}
	if len(c.AllowOrigins) != 1 || c.AllowOrigins[0] != "*" {
		t.Errorf("AllowOrigins default: %v", c.AllowOrigins)
	}
}

func TestLoad_ParseOrigins(t *testing.T) {
	t.Setenv("DATABASE_URL", "x")
	t.Setenv("LLM_API_KEY", "x")
	t.Setenv("ADMIN_API_KEY", "x")
	t.Setenv("ALLOW_ORIGINS", "https://a.my.id, https://b.my.id ,")
	c, _ := Load()
	if len(c.AllowOrigins) != 2 || c.AllowOrigins[0] != "https://a.my.id" || c.AllowOrigins[1] != "https://b.my.id" {
		t.Errorf("AllowOrigins parse: %v", c.AllowOrigins)
	}
}

func TestString_RedactsSecrets(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/db")
	t.Setenv("LLM_API_KEY", "sk-test")
	t.Setenv("ADMIN_API_KEY", "supersecret12345")
	t.Setenv("SENTRY_DSN", "https://abc123@sentry.io/456")
	t.Setenv("REDIS_URL", "rediss://default:redispass@redis-host.upstash.io:6379")
	c, _ := Load()
	s := c.String()
	if strings.Contains(s, "supersecret12345") {
		t.Errorf("admin key leaked: %s", s)
	}
	if strings.Contains(s, "pass@localhost") {
		t.Errorf("db password leaked: %s", s)
	}
	if strings.Contains(s, "abc123") {
		t.Errorf("sentry DSN leaked: %s", s)
	}
	if strings.Contains(s, "redispass") {
		t.Errorf("redis password leaked: %s", s)
	}
	if !strings.Contains(s, "redis-host.upstash.io:6379") {
		t.Errorf("redis host should stay visible: %s", s)
	}
}

func TestLoad_SentryDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "x")
	t.Setenv("LLM_API_KEY", "x")
	t.Setenv("ADMIN_API_KEY", "x")
	t.Setenv("SENTRY_DSN", "")
	t.Setenv("SENTRY_ENV", "")
	t.Setenv("SENTRY_RELEASE", "")
	t.Setenv("TRACES_SAMPLE_RATE", "")
	c, _ := Load()
	if c.SentryDSN != "" {
		t.Errorf("SentryDSN default = %q", c.SentryDSN)
	}
	if c.TracesSampleRate != 0.1 {
		t.Errorf("TracesSampleRate default = %f", c.TracesSampleRate)
	}
}

func TestLoad_TracesSampleRateParse(t *testing.T) {
	t.Setenv("DATABASE_URL", "x")
	t.Setenv("LLM_API_KEY", "x")
	t.Setenv("ADMIN_API_KEY", "x")
	t.Setenv("TRACES_SAMPLE_RATE", "0.5")
	c, _ := Load()
	if c.TracesSampleRate != 0.5 {
		t.Errorf("TracesSampleRate = %f want 0.5", c.TracesSampleRate)
	}
}

func TestLoad_ScalingKnobDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "x")
	t.Setenv("LLM_API_KEY", "x")
	t.Setenv("ADMIN_API_KEY", "x")
	t.Setenv("AGENT_RATE_LIMIT_PER_MINUTE", "")
	c, _ := Load()
	if c.AgentRateLimitPerMinute != 20 {
		t.Errorf("AgentRateLimit default = %d want 20", c.AgentRateLimitPerMinute)
	}
	if !c.BackgroundTickers {
		t.Error("BackgroundTickers default should be true (long-running hosts own delivery)")
	}
}

func TestLoad_ScalingKnobOverrides(t *testing.T) {
	t.Setenv("DATABASE_URL", "x")
	t.Setenv("LLM_API_KEY", "x")
	t.Setenv("ADMIN_API_KEY", "x")
	t.Setenv("AGENT_RATE_LIMIT_PER_MINUTE", "40")
	t.Setenv("BACKGROUND_TICKERS", "false")
	c, _ := Load()
	if c.AgentRateLimitPerMinute != 40 {
		t.Errorf("AgentRateLimit = %d want 40", c.AgentRateLimitPerMinute)
	}
	if c.BackgroundTickers {
		t.Error("BACKGROUND_TICKERS=false should disable the ticker (serverless)")
	}
}

func TestLoad_BackgroundTickersSpellings(t *testing.T) {
	for _, v := range []string{"off", "0", "no", "disabled", "OFF", "False"} {
		t.Setenv("DATABASE_URL", "x")
		t.Setenv("LLM_API_KEY", "x")
		t.Setenv("ADMIN_API_KEY", "x")
		t.Setenv("BACKGROUND_TICKERS", v)
		c, err := Load()
		if err != nil {
			t.Fatalf("BACKGROUND_TICKERS=%q: unexpected error: %v", v, err)
		}
		if c.BackgroundTickers {
			t.Errorf("BACKGROUND_TICKERS=%q should disable tickers", v)
		}
	}
}

func TestLoad_BackgroundTickersGarbageErrors(t *testing.T) {
	t.Setenv("DATABASE_URL", "x")
	t.Setenv("LLM_API_KEY", "x")
	t.Setenv("ADMIN_API_KEY", "x")
	t.Setenv("BACKGROUND_TICKERS", "maybe")
	if _, err := Load(); err == nil {
		t.Error("expected error for unrecognised BACKGROUND_TICKERS value")
	}
}

func TestLoad_ZeroRateLimitDisables(t *testing.T) {
	t.Setenv("DATABASE_URL", "x")
	t.Setenv("LLM_API_KEY", "x")
	t.Setenv("ADMIN_API_KEY", "x")
	t.Setenv("RATE_LIMIT_PER_MINUTE", "0")
	t.Setenv("AGENT_RATE_LIMIT_PER_MINUTE", "0")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.RateLimitPerMinute != 0 {
		t.Errorf("RateLimit = %d want 0 (disabled)", c.RateLimitPerMinute)
	}
	if c.AgentRateLimitPerMinute != 0 {
		t.Errorf("AgentRateLimit = %d want 0 (disabled)", c.AgentRateLimitPerMinute)
	}
}

func TestLoad_ZeroTracesDisables(t *testing.T) {
	t.Setenv("DATABASE_URL", "x")
	t.Setenv("LLM_API_KEY", "x")
	t.Setenv("ADMIN_API_KEY", "x")
	t.Setenv("TRACES_SAMPLE_RATE", "0")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.TracesSampleRate != 0 {
		t.Errorf("TracesSampleRate = %f want 0 (disabled)", c.TracesSampleRate)
	}
}

func TestLoad_PoolSizesStayPositive(t *testing.T) {
	t.Setenv("DATABASE_URL", "x")
	t.Setenv("LLM_API_KEY", "x")
	t.Setenv("ADMIN_API_KEY", "x")
	t.Setenv("DB_POOL_MAX", "0")
	t.Setenv("REPORT_MAX_BYTES", "0")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.DBPoolMax != 20 {
		t.Errorf("DBPoolMax = %d want default 20", c.DBPoolMax)
	}
	if c.ReportMaxBytes != 15000000 {
		t.Errorf("ReportMaxBytes = %d want default 15000000", c.ReportMaxBytes)
	}
}

func TestFrontendURLProductionIgnoresLocalhost(t *testing.T) {
	t.Setenv("FRONTEND_URL", "http://localhost:3000")
	if got := frontendURL("production", "https://dfdagency.vercel.app"); got != "https://dfdagency.vercel.app" {
		t.Fatalf("production frontend = %q, want public URL", got)
	}
	if got := frontendURL("development", "http://localhost:3000"); got != "http://localhost:3000" {
		t.Fatalf("development frontend = %q, want localhost", got)
	}
}

func TestLoad_ProductionRejectsWildcardCORS(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u@localhost/d")
	t.Setenv("DATABASE_ADMIN_URL", "postgres://u@localhost/d")
	t.Setenv("LLM_API_KEY", "x")
	t.Setenv("ADMIN_API_KEY", "x")
	t.Setenv("ENV", "production")
	t.Setenv("ALLOW_ORIGINS", "*")
	if _, err := Load(); err == nil {
		t.Error("expected error for wildcard CORS in production")
	}
}

func TestLoad_NonProductionAllowsWildcardCORS(t *testing.T) {
	t.Setenv("DATABASE_URL", "x")
	t.Setenv("LLM_API_KEY", "x")
	t.Setenv("ADMIN_API_KEY", "x")
	t.Setenv("ENV", "development")
	t.Setenv("ALLOW_ORIGINS", "*")
	c, err := Load()
	if err != nil {
		t.Fatalf("dev wildcard CORS must load: %v", err)
	}
	if len(c.AllowOrigins) != 1 || c.AllowOrigins[0] != "*" {
		t.Errorf("AllowOrigins = %v", c.AllowOrigins)
	}
}

func TestLoad_TrustedProxiesParsed(t *testing.T) {
	t.Setenv("DATABASE_URL", "x")
	t.Setenv("LLM_API_KEY", "x")
	t.Setenv("ADMIN_API_KEY", "x")
	t.Setenv("TRUSTED_PROXY_CIDRS", "10.0.0.0/8, 1.2.3.4")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.TrustedProxyCIDRs) != 2 {
		t.Errorf("TrustedProxyCIDRs = %v", c.TrustedProxyCIDRs)
	}
}

// baseEnv is the neutral local-development configuration the guard tests
// start from. Every test overrides only the knob it is about.
func baseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ENV", "development")
	t.Setenv("VERCEL_ENV", "")
	t.Setenv("DATABASE_URL", "postgres://u@localhost:5432/db")
	t.Setenv("DATABASE_ADMIN_URL", "")
	t.Setenv("LLM_API_KEY", "x")
	t.Setenv("ADMIN_API_KEY", "x")
	t.Setenv("RUN_MIGRATIONS", "")
	t.Setenv("ALLOW_REMOTE_DB", "")
	t.Setenv("ALLOW_ORIGINS", "*")
}

func TestLoad_RejectsInvalidENV(t *testing.T) {
	baseEnv(t)
	t.Setenv("ENV", "prod")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ENV must be") {
		t.Fatalf("expected ENV enum error, got %v", err)
	}
}

func TestLoad_RejectsRemoteDatabaseInDevelopment(t *testing.T) {
	baseEnv(t)
	t.Setenv("DATABASE_URL", "postgres://u@db.example.com:5432/db")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "remote host") {
		t.Fatalf("expected remote DB error, got %v", err)
	}
}

func TestLoad_RejectsRemoteAdminDatabaseInDevelopment(t *testing.T) {
	baseEnv(t)
	t.Setenv("DATABASE_ADMIN_URL", "postgres://u@db.example.com:5432/db")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "remote host") {
		t.Fatalf("expected remote admin DB error, got %v", err)
	}
}

func TestLoad_DevelopmentCannotOptIntoRemoteDB(t *testing.T) {
	baseEnv(t)
	t.Setenv("DATABASE_URL", "postgres://u@db.example.com:5432/db")
	t.Setenv("ALLOW_REMOTE_DB", "true")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "remote host") {
		t.Fatalf("ALLOW_REMOTE_DB must not unlock development, got %v", err)
	}
}

func TestLoad_StagingRemoteNeedsExplicitOptIn(t *testing.T) {
	baseEnv(t)
	t.Setenv("ENV", "staging")
	t.Setenv("DATABASE_URL", "postgres://u@db.example.com:5432/db")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ALLOW_REMOTE_DB") {
		t.Fatalf("staging without opt-in must fail, got %v", err)
	}

	t.Setenv("ALLOW_REMOTE_DB", "true")
	t.Setenv("EXPECTED_DB_REF", "example")
	c, err := Load()
	if err != nil {
		t.Fatalf("staging with opt-in should load: %v", err)
	}
	if c.Env != "staging" || !c.AllowRemoteDB {
		t.Fatalf("env=%q allowRemoteDB=%t", c.Env, c.AllowRemoteDB)
	}
}

func TestLoad_ProductionRequiresAdminURL(t *testing.T) {
	baseEnv(t)
	t.Setenv("ENV", "production")
	t.Setenv("ALLOW_ORIGINS", "https://dfdagency.vercel.app")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DATABASE_ADMIN_URL") {
		t.Fatalf("expected admin URL requirement, got %v", err)
	}

	t.Setenv("DATABASE_ADMIN_URL", "postgres://u@localhost:5432/db")
	if _, err := Load(); err != nil {
		t.Fatalf("production with admin URL should load: %v", err)
	}
}

func TestLoad_ExpectedDBRefRejectsAnotherEnvironment(t *testing.T) {
	baseEnv(t)
	t.Setenv("ENV", "staging")
	t.Setenv("ALLOW_REMOTE_DB", "true")
	t.Setenv("DATABASE_URL", "postgres://app_runtime.stagingref@aws-0-ap-southeast-1.pooler.supabase.com:6543/postgres")
	t.Setenv("DATABASE_ADMIN_URL", "postgres://postgres.stagingref@aws-0-ap-southeast-1.pooler.supabase.com:5432/postgres")
	t.Setenv("EXPECTED_DB_REF", "prodref")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "EXPECTED_DB_REF") {
		t.Fatalf("expected DB identity mismatch, got %v", err)
	}

	t.Setenv("EXPECTED_DB_REF", "stagingref")
	if _, err := Load(); err != nil {
		t.Fatalf("matching staging ref should load: %v", err)
	}
}

func TestDSNMatchesRef(t *testing.T) {
	cases := []struct {
		dsn  string
		ref  string
		want bool
	}{
		{"postgres://app_runtime.abc@aws-0-ap-southeast-1.pooler.supabase.com:6543/postgres", "abc", true},
		{"postgres://postgres.abc@aws-0-ap-southeast-1.pooler.supabase.com:5432/postgres", "abc", true},
		{"postgres://postgres@db.abc.supabase.co:5432/postgres", "abc", true},
		{"postgres://app_runtime.xyz@aws-0-ap-southeast-1.pooler.supabase.com:6543/postgres", "abc", false},
		{"postgres://postgres@evilabc.example.com:5432/postgres", "abc", false},
	}
	for _, tc := range cases {
		got, err := dsnMatchesRef(tc.dsn, tc.ref)
		if err != nil || got != tc.want {
			t.Errorf("dsnMatchesRef(%q, %q) = %t, %v; want %t", tc.dsn, tc.ref, got, err, tc.want)
		}
	}
}

func TestLoad_RunMigrationsDefaults(t *testing.T) {
	t.Run("development", func(t *testing.T) {
		baseEnv(t)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if !c.RunMigrations {
			t.Fatal("development should default RUN_MIGRATIONS=true")
		}
	})
	t.Run("staging", func(t *testing.T) {
		baseEnv(t)
		t.Setenv("ENV", "staging")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.RunMigrations {
			t.Fatal("staging should default RUN_MIGRATIONS=false")
		}
	})
	t.Run("production", func(t *testing.T) {
		baseEnv(t)
		t.Setenv("ENV", "production")
		t.Setenv("DATABASE_ADMIN_URL", "postgres://u@localhost:5432/db")
		t.Setenv("ALLOW_ORIGINS", "https://dfdagency.vercel.app")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.RunMigrations {
			t.Fatal("production should default RUN_MIGRATIONS=false")
		}
	})
}

func TestLoad_RejectsBootMigrationsOnVercel(t *testing.T) {
	for _, vercelEnv := range []string{"preview", "production"} {
		t.Run(vercelEnv, func(t *testing.T) {
			baseEnv(t)
			t.Setenv("VERCEL_ENV", vercelEnv)
			if vercelEnv == "production" {
				t.Setenv("ENV", "production")
				t.Setenv("DATABASE_ADMIN_URL", "postgres://u@localhost:5432/db")
				t.Setenv("ALLOW_ORIGINS", "https://dfdagency.vercel.app")
			} else {
				t.Setenv("ENV", "staging")
			}
			t.Setenv("RUN_MIGRATIONS", "true")
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "RUN_MIGRATIONS must be false on Vercel") {
				t.Fatalf("expected Vercel migration error, got %v", err)
			}
		})
	}
}

func TestLoad_VercelPreviewMustNotBeProduction(t *testing.T) {
	baseEnv(t)
	t.Setenv("VERCEL_ENV", "preview")
	t.Setenv("ENV", "production")
	t.Setenv("DATABASE_ADMIN_URL", "postgres://u@localhost:5432/db")
	t.Setenv("ALLOW_ORIGINS", "https://dfdagency.vercel.app")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "VERCEL_ENV=preview") {
		t.Fatalf("expected preview/production mismatch, got %v", err)
	}
}

func TestLoad_VercelProductionMustBeProduction(t *testing.T) {
	baseEnv(t)
	t.Setenv("VERCEL_ENV", "production")
	t.Setenv("ENV", "development")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "requires ENV=production") {
		t.Fatalf("expected Vercel production mismatch, got %v", err)
	}
}

func TestLoad_StagingPreviewLoadsWithoutMigrations(t *testing.T) {
	baseEnv(t)
	t.Setenv("VERCEL_ENV", "preview")
	t.Setenv("ENV", "staging")
	t.Setenv("RUN_MIGRATIONS", "false")
	c, err := Load()
	if err != nil {
		t.Fatalf("staging preview should load: %v", err)
	}
	if c.RunMigrations {
		t.Fatal("staging preview must not run migrations")
	}
}
