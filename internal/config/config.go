// Package config reads and validates process configuration from environment
// variables. Every binary loads the same Config so that a misconfiguration
// fails fast and identically no matter which process hits it first.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
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
	// AllowPrivateDestinations lets endpoints point at loopback, private and
	// link-local addresses. Only for local development; never in production.
	AllowPrivateDestinations bool
	// Worker holds the delivery worker's settings.
	Worker Worker
}

// Worker configures the delivery worker.
type Worker struct {
	// ID names the worker in leases and logs; empty means generated.
	ID             string
	Concurrency    int
	BatchSize      int
	PollInterval   time.Duration
	LeaseDuration  time.Duration
	RequestTimeout time.Duration
	MaxAttempts    int
	BackoffInitial time.Duration
	BackoffMax     time.Duration
	// AutoDisableAfter disables an endpoint after this many consecutive
	// failed attempts; 0 turns it off.
	AutoDisableAfter int
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

	// duration parses key and checks it lies in [lo, hi].
	duration := func(key, def string, lo, hi time.Duration) time.Duration {
		d, err := time.ParseDuration(get(key, def))
		switch {
		case err != nil:
			fail("%s: %v", key, err)
		case d < lo || d > hi:
			fail("%s must be between %s and %s, got %s", key, lo, hi, d)
		}
		return d
	}
	// integer parses key and checks it lies in [lo, hi].
	integer := func(key string, def, lo, hi int) int {
		n, err := strconv.Atoi(get(key, strconv.Itoa(def)))
		switch {
		case err != nil:
			fail("%s must be an integer", key)
		case n < lo || n > hi:
			fail("%s must be between %d and %d, got %d", key, lo, hi, n)
		}
		return n
	}

	cfg.ShutdownTimeout = duration("SHUTDOWN_TIMEOUT", "20s", time.Second, 10*time.Minute)

	switch v := strings.ToLower(get("ALLOW_PRIVATE_DESTINATIONS", "false")); v {
	case "true", "1":
		cfg.AllowPrivateDestinations = true
	case "false", "0":
	default:
		fail("ALLOW_PRIVATE_DESTINATIONS must be true or false, got %q", v)
	}

	w := Worker{
		ID:             get("WORKER_ID", ""),
		Concurrency:    integer("WORKER_CONCURRENCY", 10, 1, 1000),
		BatchSize:      integer("WORKER_BATCH_SIZE", 10, 1, 1000),
		PollInterval:   duration("WORKER_POLL_INTERVAL", "500ms", 10*time.Millisecond, time.Minute),
		LeaseDuration:  duration("WORKER_LEASE_DURATION", "60s", time.Second, time.Hour),
		RequestTimeout: duration("DELIVERY_TIMEOUT", "15s", time.Second, 5*time.Minute),
		MaxAttempts:    integer("DELIVERY_MAX_ATTEMPTS", 8, 1, 50),
		BackoffInitial: duration("DELIVERY_BACKOFF_INITIAL", "30s", 100*time.Millisecond, 24*time.Hour),
		BackoffMax:     duration("DELIVERY_BACKOFF_MAX", "6h", 100*time.Millisecond, 7*24*time.Hour),

		AutoDisableAfter: integer("ENDPOINT_AUTO_DISABLE_AFTER", 100, 0, 1_000_000),
	}
	// A lease shorter than a request (plus time to record the result) would
	// let healthy attempts be reclaimed and sent twice.
	if w.LeaseDuration > 0 && w.RequestTimeout > 0 && w.LeaseDuration < w.RequestTimeout+5*time.Second {
		fail("WORKER_LEASE_DURATION (%s) must be at least DELIVERY_TIMEOUT (%s) plus 5s", w.LeaseDuration, w.RequestTimeout)
	}
	// The worker drains for SHUTDOWN_TIMEOUT. Shorter than one request (plus
	// time to record it) and every deploy would cut off slow attempts that
	// would have finished, sending them again.
	if cfg.ShutdownTimeout > 0 && w.RequestTimeout > 0 && cfg.ShutdownTimeout < w.RequestTimeout+5*time.Second {
		fail("SHUTDOWN_TIMEOUT (%s) must be at least DELIVERY_TIMEOUT (%s) plus 5s", cfg.ShutdownTimeout, w.RequestTimeout)
	}
	if w.BackoffMax < w.BackoffInitial {
		fail("DELIVERY_BACKOFF_MAX (%s) must not be smaller than DELIVERY_BACKOFF_INITIAL (%s)", w.BackoffMax, w.BackoffInitial)
	}
	cfg.Worker = w

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return cfg, nil
}
