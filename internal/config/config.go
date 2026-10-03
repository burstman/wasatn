// Package config loads and validates application configuration from the
// environment. Secrets are only ever read from the environment and are never
// written back out (PROMPT.md: "Env only for secrets").
package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Environment is the deployment mode. Values match the SUPERKIT_ENV variable
// used by github.com/anthdm/superkit so both agree on what "production" means.
type Environment string

const (
	EnvDevelopment Environment = "development"
	EnvProduction  Environment = "production"
	EnvTest        Environment = "test"
)

// MinSessionSecretLen is the shortest acceptable SESSION_SECRET.
const MinSessionSecretLen = 32

// MetaConfig holds the Meta app credentials used by Embedded Signup and the
// Cloud API.
type MetaConfig struct {
	AppID       string
	AppSecret   string
	VerifyToken string
	// GraphVersion is the Cloud API version segment, e.g. "v21.0". Callers build
	// URLs as https://graph.facebook.com/<GraphVersion>/...
	GraphVersion string
}

// MetaConfigured reports whether Embedded Signup can be offered. Until it is,
// the connections page shows a setup notice instead of a broken button.
func (m MetaConfig) MetaConfigured() bool {
	return m.AppID != "" && m.AppSecret != ""
}

// GraphBaseURL is the Cloud API root for the configured version.
func (m MetaConfig) GraphBaseURL() string {
	version := m.GraphVersion
	if version == "" {
		version = "v21.0"
	}
	return "https://graph.facebook.com/" + version
}

// RiverConfig tunes the job worker.
type RiverConfig struct {
	// MaxWorkers is the total number of concurrent workers across all queues.
	MaxWorkers int
	// QueuePriorities maps a River queue name to its share of workers. The send
	// queue is kept separate so a flood of campaign sends cannot starve
	// maintenance jobs such as token refresh.
	Queues map[string]int
	// ConcurrencyLimit is the global rate limit in jobs/second. Per-WABA
	// throttling is enforced separately when the send worker lands.
	ConcurrencyLimit int
}

// Config is the fully validated application configuration.
type Config struct {
	Env           Environment
	HTTPAddr      string
	PublicBaseURL string
	DatabaseURL   string

	// SessionSecret signs the scs session cookie. At least 32 bytes.
	SessionSecret []byte
	// TokenKey is the 32-byte AES-256 key protecting WhatsApp access tokens.
	TokenKey []byte

	Meta       MetaConfig
	CronSecret string

	SessionCookieName  string
	SessionLifetime    time.Duration
	SessionIdleTimeout time.Duration

	// TrustProxy makes the app believe X-Forwarded-For / X-Real-IP. Only enable
	// it when the app is behind a proxy it controls, otherwise clients can spoof
	// their address and defeat per-IP rate limiting.
	TrustProxy bool

	River RiverConfig
}

// Production reports whether the app is running in production mode.
func (c *Config) Production() bool { return c.Env == EnvProduction }

// Load reads .env (if present) and then the process environment, and returns a
// validated Config. It never calls os.Exit so it stays usable from tests.
func Load() (*Config, error) {
	// A missing .env is fine in production (real env vars) and in tests.
	_ = godotenv.Load()

	env := Environment(strings.ToLower(strings.TrimSpace(getenv("SUPERKIT_ENV", string(EnvDevelopment)))))
	switch env {
	case EnvDevelopment, EnvProduction, EnvTest:
	default:
		return nil, fmt.Errorf("config: SUPERKIT_ENV must be development, production or test, got %q", env)
	}

	sessionSecret := os.Getenv("SESSION_SECRET")
	if len(sessionSecret) < MinSessionSecretLen {
		return nil, fmt.Errorf("config: SESSION_SECRET must be at least %d bytes, got %d", MinSessionSecretLen, len(sessionSecret))
	}

	tokenKey, err := parseTokenKey(os.Getenv("TOKEN_ENCRYPTION_KEY"))
	if err != nil {
		return nil, err
	}

	publicBaseURL, err := parseBaseURL(getenv("PUBLIC_BASE_URL", "http://localhost:8080"))
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Env:           env,
		HTTPAddr:      httpAddr(),
		PublicBaseURL: publicBaseURL,
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		SessionSecret: []byte(sessionSecret),
		TokenKey:      tokenKey,
		Meta: MetaConfig{
			AppID:        os.Getenv("META_APP_ID"),
			AppSecret:    os.Getenv("META_APP_SECRET"),
			VerifyToken:  os.Getenv("META_VERIFY_TOKEN"),
			GraphVersion: getenv("META_GRAPH_VERSION", "v21.0"),
		},
		CronSecret:         os.Getenv("CRON_SECRET"),
		SessionCookieName:  getenv("SESSION_COOKIE_NAME", "wasatn_session"),
		SessionLifetime:    7 * 24 * time.Hour,
		SessionIdleTimeout: 30 * time.Minute,
		TrustProxy:         getenvBool("TRUST_PROXY", false),
		River: RiverConfig{
			MaxWorkers:       getenvInt("RIVER_MAX_WORKERS", 50),
			Queues:           map[string]int{"campaign_send": 8, "campaign_schedule": 2, "default": 1},
			ConcurrencyLimit: getenvInt("RIVER_CONCURRENCY_LIMIT", 50),
		},
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// httpAddr is the listen address. HTTP_ADDR wins so local runs are explicit,
// but PaaS hosts (Render, Heroku, Fly) inject PORT and expect the app to bind
// it on all interfaces, so fall back to that before the local default.
func httpAddr() string {
	if addr := strings.TrimSpace(os.Getenv("HTTP_ADDR")); addr != "" {
		return addr
	}
	if port := strings.TrimSpace(os.Getenv("PORT")); port != "" {
		return "0.0.0.0:" + port
	}
	return ":8080"
}

// Validate reports missing values that only matter for certain features, so a
// developer can boot the app before wiring up Meta.
func (c *Config) Validate() error {
	if c.DatabaseURL == "" {
		return errors.New("config: DATABASE_URL is required")
	}
	if len(c.TokenKey) != 32 {
		return fmt.Errorf("config: TOKEN_ENCRYPTION_KEY must decode to 32 bytes, got %d", len(c.TokenKey))
	}
	if len(c.SessionSecret) < MinSessionSecretLen {
		return fmt.Errorf("config: SESSION_SECRET must be at least %d bytes, got %d", MinSessionSecretLen, len(c.SessionSecret))
	}
	if c.PublicBaseURL == "" {
		return errors.New("config: PUBLIC_BASE_URL is required")
	}
	return nil
}

// ValidateForProduction reports configuration that is unsafe outside development.
//
// Meta credentials are deliberately not required here: the app ships in
// milestones, and the connections page already degrades to a setup notice when
// they are absent. A host that is missing them logs a warning at startup
// instead of refusing to boot.
func (c *Config) ValidateForProduction() error {
	if c.Env != EnvProduction {
		return nil
	}
	if strings.HasPrefix(c.PublicBaseURL, "http://") {
		return fmt.Errorf("config: PUBLIC_BASE_URL must use https in production, got %q", c.PublicBaseURL)
	}
	if c.CronSecret == "" {
		return errors.New("config: CRON_SECRET is required in production")
	}
	return nil
}

// parseTokenKey accepts a 32-byte key as base64 (standard or URL alphabet,
// padded or raw) or as 64 hex characters.
func parseTokenKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("config: TOKEN_ENCRYPTION_KEY is required")
	}
	if len(raw) == 64 {
		if key, err := hex.DecodeString(raw); err == nil {
			return key, nil
		}
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		key, err := enc.DecodeString(raw)
		if err == nil && len(key) == 32 {
			return key, nil
		}
	}
	// Never echo the value itself: it would end up in logs.
	return nil, errors.New("config: TOKEN_ENCRYPTION_KEY must be 32 bytes encoded as base64 or hex (64 chars)")
}

// parseBaseURL accepts a full URL. A bare host with no scheme is treated as
// https, because that is what PaaS hosts hand out: Render's blueprint
// `fromService` property yields "myapp.onrender.com", not a full URL.
func parseBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw != "" && !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("config: PUBLIC_BASE_URL is invalid: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("config: PUBLIC_BASE_URL must include a scheme, got %q", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("config: PUBLIC_BASE_URL must include a host, got %q", raw)
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

func getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func getenvBool(key string, fallback bool) bool {
	raw := strings.ToLower(getenv(key, ""))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return parsed
}

func getenvInt(key string, fallback int) int {
	raw := getenv(key, "")
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}
