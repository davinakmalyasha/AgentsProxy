// DONE: PRD 7 (env config + redaction) / STANDARDS A, D
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port               string
	Env                string
	DatabaseURL        string
	DatabaseAdminURL   string
	RedisURL           string
	LLMBaseURL         string
	LLMAPIKey          string
	LLMModel           string
	AdminAPIKey        string
	MaxTokensPerReq    int
	RateLimitPerMinute int
	AllowOrigins       []string
	SentryDSN          string
	SentryEnv          string
	SentryRelease      string
	TracesSampleRate   float64
	DBPoolMin          int
	DBPoolMax          int
	DBSimpleProtocol   bool
	// Multi-bot Telegram (T3): public base URL for webhooks (empty = dev
	// polling), vault key documented in .env.example (never logged).
	PublicURL       string
	TelegramPollDev bool
	// Agent-chat per-IP sub-limit (router hardcodes 20/min today).
	AgentRateLimitPerMinute int
	// WhatsApp/notify worker queue buffer (was hardcoded 64).
	NotifyQueueSize int
	// Telegram per-bot update concurrency (was hardcoded 32).
	TelegramConcurrency int
}

func Load() (*Config, error) {
	c := &Config{
		Port:                    getenv("PORT", "8080"),
		Env:                     getenv("ENV", "development"),
		DatabaseURL:             os.Getenv("DATABASE_URL"),
		DatabaseAdminURL:        os.Getenv("DATABASE_ADMIN_URL"),
		RedisURL:                getenv("REDIS_URL", "localhost:6379"),
		LLMBaseURL:              getenv("LLM_BASE_URL", "https://api.deepseek.com"),
		LLMAPIKey:               os.Getenv("LLM_API_KEY"),
		LLMModel:                getenv("LLM_MODEL", "deepseek-chat"),
		AdminAPIKey:             os.Getenv("ADMIN_API_KEY"),
		MaxTokensPerReq:         getenvInt("MAX_TOKENS_PER_REQUEST", 2048),
		RateLimitPerMinute:      getenvInt("RATE_LIMIT_PER_MINUTE", 180),
		AllowOrigins:            splitCSV(getenv("ALLOW_ORIGINS", "*")),
		SentryDSN:               os.Getenv("SENTRY_DSN"),
		SentryEnv:               getenv("SENTRY_ENV", ""),
		SentryRelease:           getenv("SENTRY_RELEASE", ""),
		TracesSampleRate:        getenvFloat("TRACES_SAMPLE_RATE", 0.1),
		DBPoolMin:               getenvInt("DB_POOL_MIN", 2),
		DBPoolMax:               getenvInt("DB_POOL_MAX", 20),
		DBSimpleProtocol:        strings.EqualFold(getenv("DB_USE_SIMPLE_PROTOCOL", "false"), "true"),
		PublicURL:               strings.TrimRight(getenv("PROXY_PUBLIC_URL", ""), "/"),
		TelegramPollDev:         strings.EqualFold(getenv("TELEGRAM_POLL_DEV", "false"), "true"),
		AgentRateLimitPerMinute: getenvInt("AGENT_RATE_LIMIT_PER_MINUTE", 20),
		NotifyQueueSize:         getenvInt("NOTIFY_QUEUE_SIZE", 64),
		TelegramConcurrency:     getenvInt("TELEGRAM_CONCURRENCY", 32),
	}
	if c.DatabaseURL == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	if c.LLMAPIKey == "" {
		return nil, errors.New("LLM_API_KEY is required")
	}
	if c.AdminAPIKey == "" {
		return nil, errors.New("ADMIN_API_KEY is required")
	}
	return c, nil
}

func getenv(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func getenvInt(k string, def int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func getenvFloat(k string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 || f > 1 {
		return def
	}
	return f
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// String redacts secrets for safe logging.
func (c *Config) String() string {
	return fmt.Sprintf(
		"Config{port=%s env=%s db=%s db_admin=%s redis=%s llm_base=%s llm_model=%s admin_key=%s max_tokens=%d rate_limit=%d origins=%v sentry=%s sentry_env=%s sentry_release=%s traces=%.2f pool_min=%d pool_max=%d simple_proto=%t}",
		c.Port, c.Env, redactDSN(c.DatabaseURL), redactDSN(c.DatabaseAdminURL), c.RedisURL, c.LLMBaseURL, c.LLMModel,
		redact(c.AdminAPIKey, 4), c.MaxTokensPerReq, c.RateLimitPerMinute, c.AllowOrigins,
		redactDSN(c.SentryDSN), c.SentryEnv, c.SentryRelease, c.TracesSampleRate,
		c.DBPoolMin, c.DBPoolMax, c.DBSimpleProtocol,
	)
}

func redact(s string, n int) string {
	if len(s) <= n {
		return "***"
	}
	return s[:n] + "***"
}

func redactDSN(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return dsn
	}
	scheme := strings.Index(dsn, "://")
	if scheme < 0 || scheme+3 > at {
		return dsn
	}
	return dsn[:scheme+3] + "***@" + dsn[at+1:]
}
