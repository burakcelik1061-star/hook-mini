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

func TestLoadRequiresDatabaseURL(t *testing.T) {
	_, err := Load(env(nil))
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL is required") {
		t.Fatalf("err = %v, want DATABASE_URL is required", err)
	}
}
