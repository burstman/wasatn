package config

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func validKey() string {
	return strings.Repeat("ab", 32)
}

func TestParseTokenKey(t *testing.T) {
	t.Parallel()

	want := make([]byte, 32)
	for i := range want {
		want[i] = byte(i)
	}

	tests := []struct {
		name    string
		raw     string
		want    []byte
		wantErr bool
	}{
		{name: "hex", raw: hex.EncodeToString(want), want: want},
		{name: "hex uppercase", raw: strings.ToUpper(hex.EncodeToString(want)), want: want},
		{name: "std base64", raw: base64.StdEncoding.EncodeToString(want), want: want},
		{name: "raw std base64", raw: base64.RawStdEncoding.EncodeToString(want), want: want},
		{name: "url base64", raw: base64.URLEncoding.EncodeToString(want), want: want},
		{name: "raw url base64", raw: base64.RawURLEncoding.EncodeToString(want), want: want},
		{name: "surrounding whitespace", raw: "  " + base64.StdEncoding.EncodeToString(want) + "\n", want: want},
		{name: "empty", raw: "", wantErr: true},
		{name: "whitespace only", raw: "   ", wantErr: true},
		{name: "too short", raw: base64.StdEncoding.EncodeToString(want[:16]), wantErr: true},
		{name: "too long", raw: base64.StdEncoding.EncodeToString(append(want, 0)), wantErr: true},
		{name: "not encoded", raw: "this is definitely not a key", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseTokenKey(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseTokenKey() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTokenKey() error = %v", err)
			}
			if len(got) != 32 {
				t.Fatalf("key length = %d, want 32", len(got))
			}
			if string(got) != string(tc.want) {
				t.Errorf("key = %x, want %x", got, tc.want)
			}
		})
	}
}

// A malformed key must not be echoed back, because the error is logged.
func TestParseTokenKeyErrorDoesNotLeakKey(t *testing.T) {
	t.Parallel()

	const secret = "super-secret-key-material"
	_, err := parseTokenKey(secret)
	if err == nil {
		t.Fatal("parseTokenKey() error = nil, want error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error message contains the key material: %q", err.Error())
	}
}

func TestParseBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "http localhost", raw: "http://localhost:8080", want: "http://localhost:8080"},
		{name: "https with trailing slash trimmed", raw: "https://app.example.com/", want: "https://app.example.com"},
		{name: "https with path", raw: "https://example.com/app/", want: "https://example.com/app"},
		{name: "bare host from PaaS defaults to https", raw: "wasatn.onrender.com", want: "https://wasatn.onrender.com"},
		{name: "unsupported scheme", raw: "ftp://example.com", wantErr: true},
		{name: "missing host", raw: "https://", wantErr: true},
		{name: "empty", raw: "", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseBaseURL(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseBaseURL(%q) error = nil, want error", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBaseURL(%q) error = %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("parseBaseURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	newConfig := func() *Config {
		key, err := parseTokenKey(validKey())
		if err != nil {
			t.Fatalf("parseTokenKey() error = %v", err)
		}
		return &Config{
			Env:           EnvDevelopment,
			DatabaseURL:   "postgres://user:pass@localhost:5432/wasatn?sslmode=disable",
			PublicBaseURL: "http://localhost:8080",
			SessionSecret: []byte(strings.Repeat("s", MinSessionSecretLen)),
			TokenKey:      key,
		}
	}

	t.Run("valid", func(t *testing.T) {
		t.Parallel()

		if err := newConfig().Validate(); err != nil {
			t.Errorf("Validate() error = %v, want nil", err)
		}
	})

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "missing database url", mutate: func(c *Config) { c.DatabaseURL = "" }},
		{name: "short token key", mutate: func(c *Config) { c.TokenKey = []byte("too short") }},
		{name: "missing token key", mutate: func(c *Config) { c.TokenKey = nil }},
		{name: "short session secret", mutate: func(c *Config) { c.SessionSecret = []byte("short") }},
		{name: "missing session secret", mutate: func(c *Config) { c.SessionSecret = nil }},
		{name: "missing base url", mutate: func(c *Config) { c.PublicBaseURL = "" }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := newConfig()
			tc.mutate(cfg)
			if err := cfg.Validate(); err == nil {
				t.Errorf("Validate() error = nil, want error")
			}
		})
	}
}

func TestValidateForProduction(t *testing.T) {
	t.Parallel()

	newConfig := func() *Config {
		key, _ := parseTokenKey(validKey())
		return &Config{
			Env:           EnvProduction,
			DatabaseURL:   "postgres://user:pass@db.example.com/wasatn?sslmode=require",
			PublicBaseURL: "https://app.example.com",
			SessionSecret: []byte(strings.Repeat("s", MinSessionSecretLen)),
			TokenKey:      key,
			Meta: MetaConfig{
				AppID:       "123456789",
				AppSecret:   "meta-secret",
				VerifyToken: "verify-me",
			},
			CronSecret: "cron-secret",
		}
	}

	t.Run("complete production config", func(t *testing.T) {
		t.Parallel()

		if err := newConfig().ValidateForProduction(); err != nil {
			t.Errorf("ValidateForProduction() error = %v, want nil", err)
		}
	})

	t.Run("development skips production checks", func(t *testing.T) {
		t.Parallel()

		cfg := newConfig()
		cfg.Env = EnvDevelopment
		cfg.PublicBaseURL = "http://localhost:8080"
		cfg.CronSecret = ""
		if err := cfg.ValidateForProduction(); err != nil {
			t.Errorf("ValidateForProduction() error = %v, want nil in development", err)
		}
	})

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "http base url", mutate: func(c *Config) { c.PublicBaseURL = "http://app.example.com" }},
		{name: "missing cron secret", mutate: func(c *Config) { c.CronSecret = "" }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := newConfig()
			tc.mutate(cfg)
			if err := cfg.ValidateForProduction(); err == nil {
				t.Errorf("ValidateForProduction() error = nil, want error")
			}
		})
	}
}

// Meta credentials gate the connections page, not startup: an incomplete Meta
// config must not stop the app from serving auth.
func TestValidateForProductionAllowsMissingMeta(t *testing.T) {
	t.Parallel()

	key, _ := parseTokenKey(validKey())
	cfg := &Config{
		Env:           EnvProduction,
		DatabaseURL:   "postgres://user:pass@db.example.com/wasatn",
		PublicBaseURL: "https://app.example.com",
		SessionSecret: []byte(strings.Repeat("s", MinSessionSecretLen)),
		TokenKey:      key,
		CronSecret:    "cron-secret",
	}

	if err := cfg.ValidateForProduction(); err != nil {
		t.Errorf("ValidateForProduction() error = %v, want nil without Meta", err)
	}
}

func TestHTTPAddr(t *testing.T) {
	tests := []struct {
		name     string
		httpAddr string
		port     string
		want     string
	}{
		{name: "default", want: ":8080"},
		{name: "explicit HTTP_ADDR wins", httpAddr: ":9000", port: "10000", want: ":9000"},
		{name: "PORT fallback for PaaS hosts", port: "10000", want: "0.0.0.0:10000"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HTTP_ADDR", tc.httpAddr)
			t.Setenv("PORT", tc.port)

			if got := httpAddr(); got != tc.want {
				t.Errorf("httpAddr() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMetaConfigured(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		meta MetaConfig
		want bool
	}{
		{name: "both set", meta: MetaConfig{AppID: "1", AppSecret: "s"}, want: true},
		{name: "missing secret", meta: MetaConfig{AppID: "1"}, want: false},
		{name: "missing id", meta: MetaConfig{AppSecret: "s"}, want: false},
		{name: "empty", meta: MetaConfig{}, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.meta.MetaConfigured(); got != tc.want {
				t.Errorf("MetaConfigured() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGraphBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		meta MetaConfig
		want string
	}{
		{name: "default version", meta: MetaConfig{}, want: "https://graph.facebook.com/v25.0"},
		{name: "explicit version", meta: MetaConfig{GraphVersion: "v26.0"}, want: "https://graph.facebook.com/v26.0"},
		// The Facebook SDK wants "v25.0" and the REST path tolerates "25.0", so a
		// missing prefix is corrected instead of producing one of each.
		{name: "prefix added", meta: MetaConfig{GraphVersion: "25.0"}, want: "https://graph.facebook.com/v25.0"},
		{name: "whitespace trimmed", meta: MetaConfig{GraphVersion: " v26.0 "}, want: "https://graph.facebook.com/v26.0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.meta.GraphBaseURL(); got != tc.want {
				t.Errorf("GraphBaseURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGraphVersionSupported(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		meta MetaConfig
		want bool
	}{
		// An unset version means the default, which is supported by definition.
		{name: "unset uses the default", meta: MetaConfig{}, want: true},
		{name: "the default itself", meta: MetaConfig{GraphVersion: DefaultGraphVersion}, want: true},
		{name: "newer", meta: MetaConfig{GraphVersion: "v26.0"}, want: true},
		{name: "much newer", meta: MetaConfig{GraphVersion: "v99.0"}, want: true},
		// v21 predates Embedded Signup v4, which Meta refuses on these versions.
		{name: "older", meta: MetaConfig{GraphVersion: "v21.0"}, want: false},
		{name: "much older", meta: MetaConfig{GraphVersion: "v19.0"}, want: false},
		// The prefix is optional on input; Version normalises it.
		{name: "missing the v prefix", meta: MetaConfig{GraphVersion: "25.0"}, want: true},
		{name: "padded with spaces", meta: MetaConfig{GraphVersion: "  v25.0  "}, want: true},
		// These cannot be compared at all, so the operator is told rather than
		// left guessing why signup fails.
		{name: "no minor segment", meta: MetaConfig{GraphVersion: "v25"}, want: false},
		{name: "not a number", meta: MetaConfig{GraphVersion: "vNext"}, want: false},
		{name: "empty", meta: MetaConfig{GraphVersion: ""}, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.meta.GraphVersionSupported(); got != tc.want {
				t.Errorf("GraphVersionSupported() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGetenvHelpers(t *testing.T) {
	t.Setenv("TEST_STRING", "  value  ")
	t.Setenv("TEST_BOOL", "TRUE")
	t.Setenv("TEST_BOOL_JUNK", "yes-please")
	t.Setenv("TEST_INT", " 42 ")
	t.Setenv("TEST_INT_JUNK", "forty-two")
	t.Setenv("TEST_UNSET", "")

	if got := getenv("TEST_STRING", "fallback"); got != "value" {
		t.Errorf("getenv() = %q, want trimmed %q", got, "value")
	}
	if got := getenv("TEST_UNSET", "fallback"); got != "fallback" {
		t.Errorf("getenv() = %q, want fallback", got)
	}
	if got := getenvBool("TEST_BOOL", false); !got {
		t.Error("getenvBool() = false, want true")
	}
	if got := getenvBool("TEST_BOOL_JUNK", true); !got {
		t.Error("getenvBool(junk) = false, want the fallback")
	}
	if got := getenvInt("TEST_INT", 7); got != 42 {
		t.Errorf("getenvInt() = %d, want 42", got)
	}
	if got := getenvInt("TEST_INT_JUNK", 7); got != 7 {
		t.Errorf("getenvInt(junk) = %d, want the fallback", got)
	}
}

func TestLoadRejectsMissingRequiredEnv(t *testing.T) {
	// Not parallel: Load reads process-wide env and loads .env.
	t.Setenv("SUPERKIT_ENV", "test")
	t.Setenv("SESSION_SECRET", "")
	t.Setenv("TOKEN_ENCRYPTION_KEY", "")
	t.Setenv("DATABASE_URL", "")

	if _, err := Load(); err == nil {
		t.Error("Load() error = nil, want error")
	}
}

func TestLoadRejectsUnknownEnvironment(t *testing.T) {
	t.Setenv("SUPERKIT_ENV", "staging")
	t.Setenv("SESSION_SECRET", strings.Repeat("s", MinSessionSecretLen))
	t.Setenv("TOKEN_ENCRYPTION_KEY", validKey())

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "SUPERKIT_ENV") {
		t.Errorf("error = %q, want it to mention SUPERKIT_ENV", err.Error())
	}
}

func TestLoadSucceedsWithValidEnv(t *testing.T) {
	t.Setenv("SUPERKIT_ENV", "development")
	t.Setenv("SESSION_SECRET", strings.Repeat("s", MinSessionSecretLen))
	t.Setenv("TOKEN_ENCRYPTION_KEY", validKey())
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/wasatn?sslmode=disable")
	t.Setenv("PUBLIC_BASE_URL", "http://localhost:8080")
	t.Setenv("HTTP_ADDR", ":9090")
	t.Setenv("TRUST_PROXY", "true")
	t.Setenv("META_APP_ID", "123456789")
	t.Setenv("META_APP_SECRET", "meta-secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPAddr != ":9090" {
		t.Errorf("HTTPAddr = %q, want :9090", cfg.HTTPAddr)
	}
	if !cfg.TrustProxy {
		t.Error("TrustProxy = false, want true")
	}
	if cfg.Production() {
		t.Error("Production() = true, want false in development")
	}
	if len(cfg.TokenKey) != 32 {
		t.Errorf("TokenKey length = %d, want 32", len(cfg.TokenKey))
	}
	if !cfg.Meta.MetaConfigured() {
		t.Error("Meta.MetaConfigured() = false, want true; AppID and AppSecret must both be set")
	}
	if cfg.River.MaxWorkers <= 0 || len(cfg.River.Queues) == 0 {
		t.Errorf("River defaults = %+v, want workers and queues", cfg.River)
	}
}

func TestLoadRejectsShortSessionSecret(t *testing.T) {
	t.Setenv("SUPERKIT_ENV", "development")
	t.Setenv("SESSION_SECRET", "too-short")
	t.Setenv("TOKEN_ENCRYPTION_KEY", validKey())
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/wasatn?sslmode=disable")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "SESSION_SECRET") {
		t.Errorf("error = %q, want it to mention SESSION_SECRET", err.Error())
	}
}
