package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/burakcelik1061-star/hook-mini/internal/service"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// apiError is an error the client is meant to see. Every error response has
// the same envelope:
//
//	{"error": {"code": "...", "message": "...", "field": "...", "request_id": "..."}}
//
// code is stable and machine-readable; message is for humans and may change.
type apiError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

func badRequest(field, message string) *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: "invalid_request", Message: message, Field: field}
}

// writeError turns any error into a response. Errors the client caused are
// reported precisely; everything else is logged with the request ID and shown
// as a generic 500, so internals (SQL, hostnames, driver messages) never leak.
func writeError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	var apiErr *apiError
	var ve *service.ValidationError

	switch {
	case errors.As(err, &apiErr):
	case errors.As(err, &ve):
		apiErr = badRequest(ve.Field, ve.Error())
	case errors.Is(err, service.ErrNotFound):
		apiErr = &apiError{Status: http.StatusNotFound, Code: "not_found", Message: "resource not found"}
	case errors.Is(err, service.ErrUnauthorized):
		w.Header().Set("WWW-Authenticate", `Bearer realm="hook-mini"`)
		apiErr = &apiError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "invalid or missing API key"}
	case errors.Is(err, service.ErrIdempotencyMismatch):
		apiErr = &apiError{
			Status: http.StatusUnprocessableEntity, Code: "idempotency_key_reuse",
			Message: "this Idempotency-Key was already used with a different request body",
			Field:   "Idempotency-Key",
		}
	default:
		log.ErrorContext(r.Context(), "unhandled error",
			slog.String("request_id", middleware.GetReqID(r.Context())),
			slog.String("method", r.Method), slog.String("path", r.URL.Path),
			slog.Any("error", err))
		apiErr = &apiError{Status: http.StatusInternalServerError, Code: "internal_error", Message: "an unexpected error occurred"}
	}

	type body struct {
		*apiError
		RequestID string `json:"request_id,omitempty"`
	}
	writeJSON(w, apiErr.Status, map[string]any{"error": body{apiError: apiErr, RequestID: middleware.GetReqID(r.Context())}})
}
