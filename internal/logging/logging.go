// Package logging builds the process-wide structured logger.
package logging

import (
	"io"
	"log/slog"
)

// New returns a logger writing to w. The "service" attribute identifies which
// binary produced a line once logs from api and worker are interleaved.
func New(w io.Writer, service, format string, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if format == "text" {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h).With("service", service)
}
