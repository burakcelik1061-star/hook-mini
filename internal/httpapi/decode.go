package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// Request body limits. A webhook payload is capped well below what a single
// request could otherwise buffer in memory.
const (
	maxEndpointBody = 16 << 10  // 16 KiB
	maxEventBody    = 256 << 10 // 256 KiB
)

// decodeJSON reads a request body into dst and rejects anything sloppy:
// the wrong content type, an oversized or empty body, malformed JSON, fields
// we do not know (a typo like "descripton" must not be silently ignored) and
// trailing data after the object.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return &apiError{
			Status: http.StatusUnsupportedMediaType, Code: "unsupported_media_type",
			Message: "Content-Type must be application/json",
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return translateDecodeError(err, maxBytes)
	}
	// Exactly one JSON value is allowed.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return translateDecodeError(err, maxBytes)
		}
		return badRequest("body", "request body must contain a single JSON object")
	}
	return nil
}

func translateDecodeError(err error, maxBytes int64) error {
	var (
		tooBig  *http.MaxBytesError
		syntax  *json.SyntaxError
		typeErr *json.UnmarshalTypeError
	)
	switch {
	case errors.As(err, &tooBig):
		return &apiError{
			Status: http.StatusRequestEntityTooLarge, Code: "payload_too_large",
			Message: "request body is larger than the allowed " + humanBytes(maxBytes),
		}
	case errors.Is(err, io.EOF):
		return badRequest("body", "request body is empty")
	case errors.As(err, &syntax), errors.Is(err, io.ErrUnexpectedEOF):
		return badRequest("body", "request body is not valid JSON")
	case errors.As(err, &typeErr):
		field := typeErr.Field
		if field == "" {
			field = "body"
		}
		return badRequest(field, "has the wrong type: expected "+typeErr.Type.String())
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		name := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return badRequest(name, "is not a known field")
	default:
		return badRequest("body", "request body could not be read")
	}
}

func humanBytes(n int64) string {
	if n >= 1<<10 && n%(1<<10) == 0 {
		return strconv.FormatInt(n>>10, 10) + " KiB"
	}
	return strconv.FormatInt(n, 10) + " bytes"
}
