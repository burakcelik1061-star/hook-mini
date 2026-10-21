// Command worker delivers queued webhooks. It exposes no HTTP port: it shares
// state with the api only through Postgres, and any number of workers can run
// side by side.
package main

import (
	"fmt"
	"os"

	"github.com/burakcelik1061-star/hook-mini/internal/config"
	"github.com/burakcelik1061-star/hook-mini/internal/database"
	"github.com/burakcelik1061-star/hook-mini/internal/delivery"
	"github.com/burakcelik1061-star/hook-mini/internal/lifecycle"
	"github.com/burakcelik1061-star/hook-mini/internal/logging"
	"github.com/burakcelik1061-star/hook-mini/internal/retention"
	"github.com/burakcelik1061-star/hook-mini/internal/service"
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
	keys, err := cfg.RequireSecretKeys()
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

	// Every secret must open with the configured keys before any delivery is
	// attempted (see service.CheckSecrets).
	if err := service.New(pool, log, service.WithSecretKeys(keys)).CheckSecrets(ctx); err != nil {
		return err
	}

	wc := cfg.Worker
	w, err := delivery.NewWorker(pool, log, delivery.Config{
		ID:             wc.ID,
		Concurrency:    wc.Concurrency,
		BatchSize:      wc.BatchSize,
		PollInterval:   wc.PollInterval,
		LeaseDuration:  wc.LeaseDuration,
		RequestTimeout: wc.RequestTimeout,
		DrainTimeout:   cfg.ShutdownTimeout,
		Retry: delivery.RetryPolicy{
			MaxAttempts:    wc.MaxAttempts,
			InitialBackoff: wc.BackoffInitial,
			MaxBackoff:     wc.BackoffMax,
		},
		AutoDisableAfter:     wc.AutoDisableAfter,
		AllowPrivateNetworks: cfg.AllowPrivateDestinations,
		Keys:                 keys,
	})
	if err != nil {
		return err
	}
	if cfg.AllowPrivateDestinations {
		log.Warn("ALLOW_PRIVATE_DESTINATIONS is on: webhooks may be sent to private and loopback addresses; never use this in production")
	}

	cleaner, err := retention.New(pool, log, retention.Config{Period: cfg.RetentionPeriod, Interval: cfg.CleanupInterval})
	if err != nil {
		return err
	}

	log.Info("worker started",
		"worker_id", w.ID(), "concurrency", wc.Concurrency, "lease", wc.LeaseDuration,
		"request_timeout", wc.RequestTimeout, "max_attempts", wc.MaxAttempts)

	cleaned := make(chan struct{})
	go func() {
		defer close(cleaned)
		cleaner.Run(ctx)
	}()
	runErr := w.Run(ctx)
	<-cleaned // Run returned because ctx ended; the cleaner stops on the same signal
	if runErr != nil {
		return runErr
	}
	log.Info("worker stopped")
	return nil
}
