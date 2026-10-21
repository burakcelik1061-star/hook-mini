// Command migrate applies pending database migrations and exits.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/burakcelik1061-star/hook-mini/internal/config"
	"github.com/burakcelik1061-star/hook-mini/internal/database"
	"github.com/burakcelik1061-star/hook-mini/internal/lifecycle"
	"github.com/burakcelik1061-star/hook-mini/internal/logging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	log := logging.New(os.Stdout, "migrate", cfg.LogFormat, cfg.LogLevel)

	ctx, stop := lifecycle.SignalContext()
	defer stop()

	// Wait for Postgres first: in docker compose this binary can start
	// before the database accepts connections.
	pool, err := database.Connect(ctx, log, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	pool.Close()

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	n, err := database.Migrate(ctx, log, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	log.Info("migrations complete", "applied", n)
	return nil
}
