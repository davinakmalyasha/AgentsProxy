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
	t.Setenv("DATABASE_URL", "postgres://u@h/d")
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
	if c.RateLimitPerMinute != 180 {
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
	t.Setenv("DATABASE_URL", "postgres://user:pass@host:5432/db")
	t.Setenv("LLM_API_KEY", "sk-test")
	t.Setenv("ADMIN_API_KEY", "supersecret12345")
	t.Setenv("SENTRY_DSN", "https://abc123@sentry.io/456")
	c, _ := Load()
	s := c.String()
	if strings.Contains(s, "supersecret12345") {
		t.Errorf("admin key leaked: %s", s)
	}
	if strings.Contains(s, "pass@host") {
		t.Errorf("db password leaked: %s", s)
	}
	if strings.Contains(s, "abc123") {
		t.Errorf("sentry DSN leaked: %s", s)
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

func TestLoad_ProductionRejectsWildcardCORS(t *testing.T) {
	t.Setenv("DATABASE_URL", "x")
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
