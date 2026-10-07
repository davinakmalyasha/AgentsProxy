// DONE: PRD 7 (env config + redaction) / STANDARDS A, D
package config

import (
	"errors"
	"fmt"
	"math"
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
	// TrustedProxyCIDRs gates X-Forwarded-For/X-Real-IP trust. Moved
	// here from a direct os.Getenv in clientip.go so the value is
	// validated once, visible in cfg.String, and impossible to miss
	// on Vercel (where every client buckets as one IP without it).
	TrustedProxyCIDRs []string
	// LLM per-million-token prices (USD): the estimate reported to
	// every tenant. A provider swap changes the price; a recompile
	// must not be required to keep billing honest.
	CostInPerMtok  float64
	CostOutPerMtok float64
	// Daily LLM-call money caps per role: the guard on the single
	// shared LLM_API_KEY. Tunable per deploy without recompiling.
	CapCallsClient  int
	CapCallsStaff   int
	CapCallsOwner   int
	RPDCallsClient  int
	RPDCallsStaff   int
	RPDCallsOwner   int
	SentryDSN          string
	SentryEnv          string
	SentryRelease      string
	TracesSampleRate   float64
	DBPoolMin          int
	DBPoolMax          int
	DBSimpleProtocol   bool
	// Admin pool (migrations + admin endpoints + workers) talks to the
	// DIRECT 5432 connection in session mode, where Supabase caps sessions
	// (pool_size 15 on small projects). It must park almost nothing: boot
	// migrates, then only admin/worker traffic uses it. Min 0 parks zero
	// idle sessions; Max 3 bounds concurrent admin users + one worker.
	DBAdminPoolMin int
	DBAdminPoolMax int
	// Agent-chat per-IP sub-limit (router hardcodes 20/min today).
	AgentRateLimitPerMinute int
	// Durable report storage (Supabase Storage). Empty URL/key keeps the
	// local-disk backend (dev/tests). Bucket must exist; objects are
	// private and app-proxied, never public.
	SupabaseURL        string
	SupabaseStorageKey string
	ReportBucket       string
	ReportMaxBytes     int
	// BackgroundTickers runs this process's own background loops: the
	// 60s reminder ticker and the notify drain loop. Both mean the same
	// thing - "am I a long-lived process that can own background work?"
	//
	// true  = long-running hosts (Laragon, VPS, Railway). The process
	//         stays alive between ticks, so the loops actually run.
	// false = serverless (Vercel), where the function is destroyed
	//         between requests. The loops would never tick, so
	//         Supabase owns delivery instead: a pg_cron job calls the
	//         deliver-reminders Edge Function and a Database Webhook on
	//         notify_jobs INSERT handles notifications.
	//
	// Both drivers claim rows atomically (migration 030) so they can
	// never double-send; only one should be active per environment.
	// Leaving it true on serverless is not a correctness bug, but it
	// spends Active CPU on loops that can never fire.
	BackgroundTickers bool
	// RunMigrations applies embedded migrations at boot. Disable on
	// serverless (Vercel sets RUN_MIGRATIONS=false) where every cold
	// start would otherwise replay the whole ledger inside an advisory
	// lock; run migrations as a deploy step instead (make migrate /
	// Docker entrypoint / CI).
	RunMigrations bool
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
		MaxTokensPerReq:         getenvPositiveInt("MAX_TOKENS_PER_REQUEST", 2048),
		RateLimitPerMinute:      getenvInt("RATE_LIMIT_PER_MINUTE", 180),
		AllowOrigins:            splitCSV(getenv("ALLOW_ORIGINS", "*")),
		TrustedProxyCIDRs:     splitCSV(getenv("TRUSTED_PROXY_CIDRS", "")),
		CostInPerMtok:         getenvFloatUnbounded("LLM_COST_IN_PER_MTOK", 0.14),
		CostOutPerMtok:        getenvFloatUnbounded("LLM_COST_OUT_PER_MTOK", 0.28),
		CapCallsClient:        getenvInt("CAP_CALLS_CLIENT", 20),
		CapCallsStaff:         getenvInt("CAP_CALLS_PEGAWAI", 80),
		CapCallsOwner:         getenvInt("CAP_CALLS_OWNER", 150),
		RPDCallsClient:        getenvInt("RPD_CALLS_CLIENT", 30),
		RPDCallsStaff:         getenvInt("RPD_CALLS_PEGAWAI", 80),
		RPDCallsOwner:         getenvInt("RPD_CALLS_OWNER", 150),
		SentryDSN:               os.Getenv("SENTRY_DSN"),
		SentryEnv:               getenv("SENTRY_ENV", ""),
		SentryRelease:           getenv("SENTRY_RELEASE", ""),
		TracesSampleRate:        getenvFloat("TRACES_SAMPLE_RATE", 0.1),
		DBPoolMin:               getenvPositiveInt("DB_POOL_MIN", 2),
		DBPoolMax:               getenvPositiveInt("DB_POOL_MAX", 20),
		DBSimpleProtocol:        false, // set below (strict parse, may error)
		DBAdminPoolMin:          getenvPositiveInt("DB_ADMIN_POOL_MIN", 0),
		DBAdminPoolMax:          getenvPositiveInt("DB_ADMIN_POOL_MAX", 3),
		AgentRateLimitPerMinute: getenvInt("AGENT_RATE_LIMIT_PER_MINUTE", 20),
		SupabaseURL:             strings.TrimRight(getenv("SUPABASE_URL", ""), "/"),
		SupabaseStorageKey:      os.Getenv("SUPABASE_STORAGE_KEY"),
		ReportBucket:            getenv("REPORT_BUCKET", "reports"),
		ReportMaxBytes:          getenvPositiveInt("REPORT_MAX_BYTES", 15000000),
		BackgroundTickers:       true, // set below (strict parse, may error)
	}
	// Strict booleans: an unrecognised value is a boot error, not a
	// silent default. BACKGROUND_TICKERS=off used to leave the loops ON
	// (only the literal "false" disabled them), burning Active CPU on
	// serverless while the operator believed they were off.
	var err error
	if c.BackgroundTickers, err = getenvBool("BACKGROUND_TICKERS", true); err != nil {
		return nil, err
	}
	if c.RunMigrations, err = getenvBool("RUN_MIGRATIONS", true); err != nil {
		return nil, err
	}
	if c.DBSimpleProtocol, err = getenvBool("DB_USE_SIMPLE_PROTOCOL", false); err != nil {
		return nil, err
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
	// ENV=production tightens CORS: a wildcard origin in production is
	// a quota-burn hole (any site can call the public endpoints), so it
	// fails boot instead of serving. Documented in .env.example.
	if strings.EqualFold(c.Env, "production") {
		for _, o := range c.AllowOrigins {
			if o == "*" {
				return nil, errors.New("ALLOW_ORIGINS must not contain '*' when ENV=production")
			}
		}
	}
	return c, nil
}

func getenv(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// getenvInt parses an integer knob where 0 is meaningful (e.g. a rate
// limit where 0 disables the check). Negative or unparsable values fall
// back to def.
func getenvInt(k string, def int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// getenvPositiveInt parses an integer knob where 0 is not a valid value
// (pool sizes, byte caps). Zero, negative, or unparsable values fall back
// to def, so a kill switch can never be expressed — and can never be
// thrown by accident — through these knobs.
func getenvPositiveInt(k string, def int) int {
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

// getenvBool parses a boolean knob strictly. Empty means def; recognised
// true/false spellings (case-insensitive, with common synonyms) apply;
// anything else is an error so a typo can never silently enable
// background CPU burn or silently disable a protocol flag.
func getenvBool(k string, def bool) (bool, error) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(k)))
	if v == "" {
		return def, nil
	}
	switch v {
	case "1", "true", "yes", "y", "on", "enable", "enabled":
		return true, nil
	case "0", "false", "no", "n", "off", "disable", "disabled":
		return false, nil
	default:
		return def, fmt.Errorf("invalid boolean for %s: %q", k, strings.TrimSpace(os.Getenv(k)))
	}
}

func getenvFloat(k string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	// Exactly 0 is valid (disables sampling); NaN never is.
	if err != nil || math.IsNaN(f) || f < 0 || f > 1 {
		return def
	}
	return f
}

// getenvFloatUnbounded parses a non-negative float with no upper bound
// (prices, timeouts-as-seconds). Empty, negative, NaN, +Inf, or
// unparsable values fall back to def.
func getenvFloatUnbounded(k string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
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
		"Config{port=%s env=%s db=%s db_admin=%s redis=%s llm_base=%s llm_model=%s admin_key=%s max_tokens=%d rate_limit=%d origins=%v proxies=%v sentry=%s sentry_env=%s sentry_release=%s traces=%.2f pool_min=%d pool_max=%d simple_proto=%t reports=%s/%s max_report_bytes=%d BACKGROUND_TICKERS=%t RUN_MIGRATIONS=%t}",
		c.Port, c.Env, redactDSN(c.DatabaseURL), redactDSN(c.DatabaseAdminURL), c.RedisURL, c.LLMBaseURL, c.LLMModel,
		redact(c.AdminAPIKey, 4), c.MaxTokensPerReq, c.RateLimitPerMinute, c.AllowOrigins, c.TrustedProxyCIDRs,
		redactDSN(c.SentryDSN), c.SentryEnv, c.SentryRelease, c.TracesSampleRate,
		c.DBPoolMin, c.DBPoolMax, c.DBSimpleProtocol, reportBackend(c), c.ReportBucket, c.ReportMaxBytes, c.BackgroundTickers, c.RunMigrations,
	)
}

// reportBackend names the report backend without leaking the key.
func reportBackend(c *Config) string {
	if c.SupabaseURL != "" && c.SupabaseStorageKey != "" {
		return "supabase"
	}
	return "local"
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
