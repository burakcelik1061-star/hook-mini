// Package config reads and validates process configuration from environment
// variables. Every binary loads the same Config so that a misconfiguration
// fails fast and identically no matter which process hits it first.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
)

// Config is the validated configuration shared by the api, worker and migrate
// binaries. Fields that only one binary needs are still validated here, which
// keeps a single source of truth for defaults.
type Config struct {
	// DatabaseURL is a Postgres connection string (postgres:// or postgresql://).
	DatabaseURL string
	// HTTPAddr is the listen address of the api binary.
	HTTPAddr string
	// LogLevel is the minimum level that is emitted.
	LogLevel slog.Level
	// LogFormat is "json" (default, for production) or "text" (for terminals).
	LogFormat string
	// ShutdownTimeout bounds how long a process waits for in-flight work to
	// finish after receiving a termination signal.
	ShutdownTimeout time.Duration
}

// Load builds a Config from getenv (usually os.Getenv). All problems are
// reported together so an operator fixes the environment once, not one
// variable per restart.
func Load(getenv func(string) string) (Config, error) {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}

	cfg := Config{
		DatabaseURL: get("DATABASE_URL", ""),
		HTTPAddr:    get("HTTP_ADDR", ":8080"),
		LogFormat:   strings.ToLower(get("LOG_FORMAT", "json")),
	}

	if cfg.DatabaseURL == "" {
		fail("DATABASE_URL is required")
	} else if u, err := url.Parse(cfg.DatabaseURL); err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		fail("DATABASE_URL must be a postgres:// or postgresql:// URL")
	}

	if err := cfg.LogLevel.UnmarshalText([]byte(get("LOG_LEVEL", "info"))); err != nil {
		fail("LOG_LEVEL: %v", err)
	}

	if cfg.LogFormat != "json" && cfg.LogFormat != "text" {
		fail("LOG_FORMAT must be \"json\" or \"text\", got %q", cfg.LogFormat)
	}

	d, err := time.ParseDuration(get("SHUTDOWN_TIMEOUT", "15s"))
	switch {
	case err != nil:
		fail("SHUTDOWN_TIMEOUT: %v", err)
	case d <= 0:
		fail("SHUTDOWN_TIMEOUT must be positive, got %s", d)
	default:
		cfg.ShutdownTimeout = d
	}

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return cfg, nil
}
