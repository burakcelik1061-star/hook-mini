// Package httpapi wires the HTTP surface of the api binary.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Pinger is the one thing the HTTP layer needs from the database: proof that
// it is reachable. Depending on an interface keeps handlers testable without
// a real Postgres.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Deps are the collaborators the router is built from.
type Deps struct {
	Log *slog.Logger
	DB  Pinger
}

// NewRouter returns the root handler. Phase 0 exposes only the operational
// endpoints; the public API is mounted here in later phases.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(requestLogger(d.Log))
	r.Use(middleware.Recoverer)

	r.Get("/healthz", healthz)
	r.Get("/readyz", readyz(d.Log, d.DB))

	return r
}

// healthz is the liveness probe: the process is up and serving. It must not
// touch dependencies, otherwise a database outage would make an orchestrator
// restart healthy processes.
func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz is the readiness probe: the process can do useful work, which needs
// the database.
func readyz(log *slog.Logger, db Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			// The cause stays in the logs; the response must not echo it.
			log.WarnContext(r.Context(), "readiness check failed",
				slog.String("request_id", middleware.GetReqID(r.Context())),
				slog.Any("error", err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "unavailable",
				"reason": "database unreachable",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// requestLogger emits one structured line per request, carrying the request
// ID so a log line can be tied back to a client report.
func requestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			level := slog.LevelInfo
			if status >= 500 {
				level = slog.LevelError
			}
			log.LogAttrs(r.Context(), level, "http request",
				slog.String("request_id", middleware.GetReqID(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Duration("duration", time.Since(start)),
			)
		})
	}
}
