// Package config loads process configuration from the environment.
//
// Secrets are read only from the environment (or files referenced by *_FILE
// variables, for Docker/Kubernetes secrets) and are never logged: Config's
// String method redacts them.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Env names the deployment environment. Production enables the strictest
// defaults (secure cookies, no debug output).
type Env string

const (
	EnvDevelopment Env = "development"
	EnvTest        Env = "test"
	EnvStaging     Env = "staging"
	EnvProduction  Env = "production"
)

// Config is the full process configuration. Every field is set by Load.
type Config struct {
	Env Env

	// DatabaseURL is the runtime connection string. It must use a role with
	// DML-only grants; MigrationDatabaseURL uses the owner role for DDL.
	DatabaseURL          Secret
	MigrationDatabaseURL Secret
	DBMaxConns           int32

	HTTPAddr          string
	HTTPReadTimeout   time.Duration
	HTTPWriteTimeout  time.Duration
	HTTPIdleTimeout   time.Duration
	MaxRequestBytes   int64
	MaxUploadBytes    int64
	TrustedProxyCIDRs []string
	CORSOrigins       []string

	SessionIdleTTL     time.Duration
	SessionAbsoluteTTL time.Duration
	CookieDomain       string

	// SecretKey derives HMAC keys (CSRF tokens, cursor signing). 32+ bytes.
	SecretKey Secret

	BlobEndpoint  string
	BlobBucket    string
	BlobAccessKey Secret
	BlobSecretKey Secret
	BlobUseTLS    bool
	// BlobSSE requests server-side encryption on every object. Required
	// outside development; local SeaweedFS may run without it.
	BlobSSE bool

	MetricsAddr string
	LogLevel    string
	Timezone    *time.Location
}

// Secret holds a sensitive string. Its String and GoString methods redact the
// value so it cannot leak through fmt or slog.
type Secret string

func (Secret) String() string   { return "[redacted]" }
func (Secret) GoString() string { return "[redacted]" }

// LogValue redacts the secret for slog.
func (Secret) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// Reveal returns the raw value. Call it only where the secret is consumed.
func (s Secret) Reveal() string { return string(s) }

// IsProduction reports whether the strictest settings apply.
func (c Config) IsProduction() bool { return c.Env == EnvProduction || c.Env == EnvStaging }

// Load reads configuration from the environment and validates it.
func Load() (Config, error) {
	return load(os.LookupEnv)
}

type lookupFunc func(string) (string, bool)

func load(lookup lookupFunc) (Config, error) {
	r := reader{lookup: lookup}
	c := Config{
		Env:                  Env(r.str("APP_ENV", string(EnvDevelopment))),
		DatabaseURL:          r.secret("DATABASE_URL", true),
		MigrationDatabaseURL: r.secret("MIGRATION_DATABASE_URL", false),
		DBMaxConns:           r.int32("DB_MAX_CONNS", 20, 1, 1000),
		HTTPAddr:             r.str("HTTP_ADDR", ":8080"),
		HTTPReadTimeout:      r.duration("HTTP_READ_TIMEOUT", 15*time.Second),
		HTTPWriteTimeout:     r.duration("HTTP_WRITE_TIMEOUT", 30*time.Second),
		HTTPIdleTimeout:      r.duration("HTTP_IDLE_TIMEOUT", 60*time.Second),
		MaxRequestBytes:      int64(r.int("MAX_REQUEST_BYTES", 1<<20)),
		MaxUploadBytes:       int64(r.int("MAX_UPLOAD_BYTES", 20<<20)),
		TrustedProxyCIDRs:    r.list("TRUSTED_PROXY_CIDRS"),
		CORSOrigins:          r.list("CORS_ORIGINS"),
		SessionIdleTTL:       r.duration("SESSION_IDLE_TTL", 30*time.Minute),
		SessionAbsoluteTTL:   r.duration("SESSION_ABSOLUTE_TTL", 12*time.Hour),
		CookieDomain:         r.str("COOKIE_DOMAIN", ""),
		SecretKey:            r.secret("SECRET_KEY", true),
		BlobEndpoint:         r.str("BLOB_ENDPOINT", ""),
		BlobBucket:           r.str("BLOB_BUCKET", ""),
		BlobAccessKey:        r.secret("BLOB_ACCESS_KEY", false),
		BlobSecretKey:        r.secret("BLOB_SECRET_KEY", false),
		BlobUseTLS:           r.bool("BLOB_USE_TLS", true),
		BlobSSE:              r.bool("BLOB_SSE", true),
		MetricsAddr:          r.str("METRICS_ADDR", ":9090"),
		LogLevel:             r.str("LOG_LEVEL", "info"),
	}
	tz := r.str("APP_TIMEZONE", "Asia/Kolkata")
	loc, err := time.LoadLocation(tz)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("APP_TIMEZONE: %w", err))
	}
	c.Timezone = loc

	if c.MigrationDatabaseURL == "" {
		c.MigrationDatabaseURL = c.DatabaseURL
	}
	r.errs = append(r.errs, c.validate()...)
	if len(r.errs) > 0 {
		return Config{}, errors.Join(r.errs...)
	}
	return c, nil
}

func (c Config) validate() []error {
	var errs []error
	switch c.Env {
	case EnvDevelopment, EnvTest, EnvStaging, EnvProduction:
	default:
		errs = append(errs, fmt.Errorf("APP_ENV: unknown environment %q", c.Env))
	}
	if len(c.SecretKey) < 32 {
		errs = append(errs, errors.New("SECRET_KEY: must be at least 32 bytes"))
	}
	if c.SessionIdleTTL <= 0 || c.SessionAbsoluteTTL < c.SessionIdleTTL {
		errs = append(errs, errors.New("SESSION_*_TTL: idle must be positive and not exceed absolute"))
	}
	for _, o := range c.CORSOrigins {
		u, err := url.Parse(o)
		if err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" || o == "*" {
			errs = append(errs, fmt.Errorf("CORS_ORIGINS: %q must be an exact origin like https://core.example.com", o))
			continue
		}
		if c.IsProduction() && u.Scheme != "https" {
			errs = append(errs, fmt.Errorf("CORS_ORIGINS: %q must use https outside development", o))
		}
	}
	if c.IsProduction() {
		for name, raw := range map[string]Secret{"DATABASE_URL": c.DatabaseURL, "MIGRATION_DATABASE_URL": c.MigrationDatabaseURL} {
			if !requiresVerifiedTLS(raw.Reveal()) {
				errs = append(errs, fmt.Errorf("%s: must set sslmode=verify-full outside development", name))
			}
		}
	}
	if c.IsProduction() && !c.BlobUseTLS {
		errs = append(errs, errors.New("BLOB_USE_TLS: must be true outside development"))
	}
	if c.IsProduction() && !c.BlobSSE {
		errs = append(errs, errors.New("BLOB_SSE: must be true outside development"))
	}
	return errs
}

// requiresVerifiedTLS reports whether a Postgres URL demands a verified TLS
// connection. The URL is never included in errors: it carries a password.
func requiresVerifiedTLS(dsn string) bool {
	u, err := url.Parse(dsn)
	return err == nil && u.Query().Get("sslmode") == "verify-full"
}

type reader struct {
	lookup lookupFunc
	errs   []error
}

func (r *reader) raw(key string) (string, bool) {
	if v, ok := r.lookup(key); ok {
		return strings.TrimSpace(v), true
	}
	// Support Docker/Kubernetes secrets mounted as files.
	if path, ok := r.lookup(key + "_FILE"); ok && path != "" {
		b, err := os.ReadFile(path) //nolint:gosec // path comes from operator-controlled env.
		if err != nil {
			r.errs = append(r.errs, fmt.Errorf("%s_FILE: %w", key, err))
			return "", false
		}
		return strings.TrimSpace(string(b)), true
	}
	return "", false
}

func (r *reader) str(key, def string) string {
	if v, ok := r.raw(key); ok && v != "" {
		return v
	}
	return def
}

func (r *reader) secret(key string, required bool) Secret {
	v, ok := r.raw(key)
	if (!ok || v == "") && required {
		r.errs = append(r.errs, fmt.Errorf("%s: required", key))
	}
	return Secret(v)
}

func (r *reader) int(key string, def int) int {
	v, ok := r.raw(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return n
}

// int32 reads an integer that must lie in [lo, hi].
func (r *reader) int32(key string, def, lo, hi int32) int32 {
	n := r.int(key, int(def))
	if n < int(lo) || n > int(hi) {
		r.errs = append(r.errs, fmt.Errorf("%s: must be between %d and %d", key, lo, hi))
		return def
	}
	return int32(n) //nolint:gosec // bounds checked above.
}

func (r *reader) bool(key string, def bool) bool {
	v, ok := r.raw(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return b
}

func (r *reader) duration(key string, def time.Duration) time.Duration {
	v, ok := r.raw(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return d
}

func (r *reader) list(key string) []string {
	v, ok := r.raw(key)
	if !ok || v == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
