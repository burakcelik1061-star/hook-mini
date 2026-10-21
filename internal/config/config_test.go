package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL": "postgres://u:p@localhost:5432/hookmini",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want json", cfg.LogFormat)
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 15s", cfg.ShutdownTimeout)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL":     "postgresql://u:p@db/hookmini?sslmode=disable",
		"HTTP_ADDR":        "127.0.0.1:9000",
		"LOG_LEVEL":        "debug",
		"LOG_FORMAT":       "TEXT",
		"SHUTDOWN_TIMEOUT": "2s",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != "127.0.0.1:9000" || cfg.LogLevel != slog.LevelDebug ||
		cfg.LogFormat != "text" || cfg.ShutdownTimeout != 2*time.Second {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestLoadReportsEveryProblem(t *testing.T) {
	_, err := Load(env(map[string]string{
		"DATABASE_URL":     "mysql://nope",
		"LOG_LEVEL":        "loud",
		"LOG_FORMAT":       "xml",
		"SHUTDOWN_TIMEOUT": "-1s",
	}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"DATABASE_URL", "LOG_LEVEL", "LOG_FORMAT", "SHUTDOWN_TIMEOUT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestWorkerDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{"DATABASE_URL": "postgres://u:p@db/x"}))
	if err != nil {
		t.Fatal(err)
	}
	want := Worker{
		Concurrency: 10, BatchSize: 10, PollInterval: 500 * time.Millisecond,
		LeaseDuration: 60 * time.Second, RequestTimeout: 15 * time.Second,
		MaxAttempts: 8, BackoffInitial: 30 * time.Second, BackoffMax: 6 * time.Hour,
	}
	if cfg.Worker != want {
		t.Errorf("worker defaults = %+v, want %+v", cfg.Worker, want)
	}
}

func TestWorkerOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL":             "postgres://u:p@db/x",
		"WORKER_ID":                "w-1",
		"WORKER_CONCURRENCY":       "32",
		"WORKER_BATCH_SIZE":        "16",
		"WORKER_POLL_INTERVAL":     "250ms",
		"WORKER_LEASE_DURATION":    "2m",
		"DELIVERY_TIMEOUT":         "30s",
		"DELIVERY_MAX_ATTEMPTS":    "3",
		"DELIVERY_BACKOFF_INITIAL": "1s",
		"DELIVERY_BACKOFF_MAX":     "1m",
	}))
	if err != nil {
		t.Fatal(err)
	}
	w := cfg.Worker
	if w.ID != "w-1" || w.Concurrency != 32 || w.BatchSize != 16 || w.PollInterval != 250*time.Millisecond ||
		w.LeaseDuration != 2*time.Minute || w.RequestTimeout != 30*time.Second || w.MaxAttempts != 3 ||
		w.BackoffInitial != time.Second || w.BackoffMax != time.Minute {
		t.Errorf("unexpected worker config: %+v", w)
	}
}

func TestWorkerValidation(t *testing.T) {
	tests := map[string]struct {
		env  map[string]string
		want string
	}{
		"concurrency not a number": {map[string]string{"WORKER_CONCURRENCY": "lots"}, "WORKER_CONCURRENCY must be an integer"},
		"concurrency zero":         {map[string]string{"WORKER_CONCURRENCY": "0"}, "WORKER_CONCURRENCY must be between"},
		"attempts too many":        {map[string]string{"DELIVERY_MAX_ATTEMPTS": "500"}, "DELIVERY_MAX_ATTEMPTS must be between"},
		"poll too fast":            {map[string]string{"WORKER_POLL_INTERVAL": "1ms"}, "WORKER_POLL_INTERVAL must be between"},
		"timeout unparsable":       {map[string]string{"DELIVERY_TIMEOUT": "soon"}, "DELIVERY_TIMEOUT"},
		"lease shorter than request": {
			map[string]string{"WORKER_LEASE_DURATION": "10s", "DELIVERY_TIMEOUT": "15s"},
			"WORKER_LEASE_DURATION (10s) must be at least DELIVERY_TIMEOUT (15s) plus 5s",
		},
		"backoff max below initial": {
			map[string]string{"DELIVERY_BACKOFF_INITIAL": "1h", "DELIVERY_BACKOFF_MAX": "1m"},
			"DELIVERY_BACKOFF_MAX (1m0s) must not be smaller than DELIVERY_BACKOFF_INITIAL (1h0m0s)",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			e := map[string]string{"DATABASE_URL": "postgres://u:p@db/x"}
			for k, v := range tt.env {
				e[k] = v
			}
			_, err := Load(env(e))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	_, err := Load(env(nil))
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL is required") {
		t.Fatalf("err = %v, want DATABASE_URL is required", err)
	}
}
