// Package httpapi wires the HTTP surface of the api binary.
package httpapi

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/burakcelik1061-star/hook-mini/api"
	"github.com/burakcelik1061-star/hook-mini/internal/ratelimit"
	"github.com/burakcelik1061-star/hook-mini/internal/service"
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
	// Svc backs the /v1 API. When nil only the operational endpoints are
	// served, which keeps the health handlers testable without a database.
	Svc *service.Service
	// Limiter caps each tenant's request rate on /v1. Nil means no limit.
	Limiter *ratelimit.Limiter
}

// NewRouter returns the root handler.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(requestID)
	r.Use(requestLogger(d.Log))
	r.Use(recoverer(d.Log))

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		writeError(w, req, d.Log, &apiError{Status: http.StatusNotFound, Code: "not_found", Message: "no such route"})
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		writeError(w, req, d.Log, &apiError{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed", Message: "method not allowed for this route"})
	})

	r.Get("/healthz", healthz)
	r.Get("/readyz", readyz(d.Log, d.DB))
	r.Get("/openapi.yaml", openAPI)

	if d.Svc != nil {
		h := &handlers{svc: d.Svc, log: d.Log, limiter: d.Limiter}
		r.Route("/v1", func(r chi.Router) {
			r.Use(apiHeaders, h.authenticate, h.rateLimit)

			r.Route("/endpoints", func(r chi.Router) {
				r.Post("/", h.createEndpoint)
				r.Get("/", h.listEndpoints)
				r.Get("/{id}", h.getEndpoint)
				r.Patch("/{id}", h.updateEndpoint)
				r.Delete("/{id}", h.deleteEndpoint)
				r.Post("/{id}/rotate-secret", h.rotateSecret)
				r.Post("/{id}/disable", h.disableEndpoint)
				r.Post("/{id}/enable", h.enableEndpoint)
			})
			r.Route("/deliveries", func(r chi.Router) {
				r.Get("/", h.listDeliveries)
				r.Get("/{id}", h.getDelivery)
				r.Post("/{id}/replay", h.replayDelivery)
			})
			r.Route("/events", func(r chi.Router) {
				r.Post("/", h.createEvent)
				r.Get("/", h.listEvents)
				r.Get("/{id}", h.getEvent)
			})
		})
	}

	return r
}

// openAPI serves the embedded OpenAPI document. It is public: it describes the
// API, it does not expose any data.
func openAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(api.OpenAPI)
}

type requestIDKey struct{}

// requestID gives every request an ID of its own and returns it in the
// X-Request-Id header, so a client can quote it in a report. An incoming
// X-Request-Id is deliberately not reused: it is client input of any length
// and content, and trusting it would let a client write what it likes into
// every log line and pass its requests off under another request's ID. The
// ID is random (no host name, unlike chi's generator, which would publish the
// container's name).
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := rand.Text()
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// requestIDOf returns the ID requestID gave r.
func requestIDOf(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey{}).(string)
	return id
}

// recoverer turns a panic in a handler into the documented 500 error
// envelope, not a bare 500, and logs it with the request ID and the stack.
// http.ErrAbortHandler is passed on: it is how a handler asks net/http to
// abort the response, not a failure.
func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.ErrorContext(r.Context(), "panic while serving a request",
					slog.String("request_id", requestIDOf(r)),
					slog.String("method", r.Method), slog.String("path", r.URL.Path),
					slog.Any("panic", v), slog.String("stack", string(debug.Stack())))
				if ww, ok := w.(middleware.WrapResponseWriter); ok && ww.Status() != 0 {
					return // the response has already begun; it cannot be replaced
				}
				writeAPIError(w, r, internalError())
			}()
			next.ServeHTTP(w, r)
		})
	}
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
				slog.String("request_id", requestIDOf(r)),
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
				slog.String("request_id", requestIDOf(r)),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Duration("duration", time.Since(start)),
			)
		})
	}
}
