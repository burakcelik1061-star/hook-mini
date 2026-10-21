// Package lifecycle holds the start-up and shutdown plumbing shared by the
// binaries, so each main stays a short, readable composition root.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// SignalContext returns a context that is cancelled on SIGINT or SIGTERM
// (docker stop sends SIGTERM). Calling the returned stop function restores
// default signal behaviour, so a second Ctrl+C kills a stuck shutdown.
func SignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// Serve runs srv on ln until ctx is cancelled, then drains in-flight requests
// for at most timeout. It returns nil on a clean shutdown.
func Serve(ctx context.Context, srv *http.Server, ln net.Listener, timeout time.Duration) error {
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		// Serve only returns on its own when something is wrong.
		return fmt.Errorf("http server stopped unexpectedly: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}
