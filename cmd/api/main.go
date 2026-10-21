// Command api serves the Hook Mini HTTP API.
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/burakcelik1061-star/hook-mini/internal/config"
	"github.com/burakcelik1061-star/hook-mini/internal/database"
	"github.com/burakcelik1061-star/hook-mini/internal/httpapi"
	"github.com/burakcelik1061-star/hook-mini/internal/lifecycle"
	"github.com/burakcelik1061-star/hook-mini/internal/logging"
	"github.com/burakcelik1061-star/hook-mini/internal/service"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "api:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	log := logging.New(os.Stdout, "api", cfg.LogFormat, cfg.LogLevel)

	ctx, stop := lifecycle.SignalContext()
	defer stop()

	pool, err := database.Connect(ctx, log, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.HTTPAddr, err)
	}
	srv := &http.Server{
		Handler:           httpapi.NewRouter(httpapi.Deps{Log: log, DB: pool, Svc: service.New(pool, log)}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		// No BaseContext tied to the signal context: cancelling request
		// contexts at SIGTERM would abort the very requests Shutdown is
		// meant to let finish.
	}

	log.Info("api listening", "addr", ln.Addr().String())
	if err := lifecycle.Serve(ctx, srv, ln, cfg.ShutdownTimeout); err != nil {
		return err
	}
	log.Info("api stopped")
	return nil
}
