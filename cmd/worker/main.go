// Command worker delivers queued webhooks. It exposes no HTTP port: it shares
// state with the api only through Postgres.
package main

import (
	"fmt"
	"os"

	"github.com/burakcelik1061-star/hook-mini/internal/config"
	"github.com/burakcelik1061-star/hook-mini/internal/database"
	"github.com/burakcelik1061-star/hook-mini/internal/lifecycle"
	"github.com/burakcelik1061-star/hook-mini/internal/logging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	log := logging.New(os.Stdout, "worker", cfg.LogFormat, cfg.LogLevel)

	ctx, stop := lifecycle.SignalContext()
	defer stop()

	pool, err := database.Connect(ctx, log, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// The delivery loop arrives in Phase 3. Until then the worker only proves
	// that it can start, reach the database and stop cleanly on a signal.
	log.Info("worker started; delivery loop not implemented yet")
	<-ctx.Done()
	log.Info("worker stopping")
	return nil
}
