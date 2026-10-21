package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

func newTestRouter(db Pinger) http.Handler {
	return NewRouter(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: db})
}

func get(t *testing.T, h http.Handler, path string) (*httptest.ResponseRecorder, map[string]string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (%q)", err, rec.Body.String())
	}
	return rec, body
}

func TestHealthzDoesNotTouchDatabase(t *testing.T) {
	rec, body := get(t, newTestRouter(fakeDB{err: errors.New("down")}), "/healthz")
	if rec.Code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("got %d %v, want 200 ok even when the database is down", rec.Code, body)
	}
}

func TestReadyz(t *testing.T) {
	tests := []struct {
		name       string
		db         Pinger
		wantStatus int
		wantBody   string
	}{
		{"database up", fakeDB{}, http.StatusOK, "ready"},
		{"database down", fakeDB{err: errors.New("connection refused")}, http.StatusServiceUnavailable, "unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, body := get(t, newTestRouter(tt.db), "/readyz")
			if rec.Code != tt.wantStatus || body["status"] != tt.wantBody {
				t.Fatalf("got %d %v, want %d %s", rec.Code, body, tt.wantStatus, tt.wantBody)
			}
			if tt.wantStatus != http.StatusOK && body["reason"] == "" {
				t.Error("an unready response should say why")
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Errorf("Content-Type = %q", ct)
			}
		})
	}
}

func TestReadyzDoesNotLeakDriverErrors(t *testing.T) {
	rec, _ := get(t, newTestRouter(fakeDB{err: errors.New("password authentication failed for user hookmini")}), "/readyz")
	if body := rec.Body.String(); strings.Contains(body, "password") || strings.Contains(body, "hookmini") {
		t.Fatalf("readiness response leaks internals: %s", body)
	}
}

func TestReadyzLogsTheCause(t *testing.T) {
	var buf bytes.Buffer
	h := NewRouter(Deps{Log: slog.New(slog.NewTextHandler(&buf, nil)), DB: fakeDB{err: errors.New("connection refused")}})
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if !strings.Contains(buf.String(), "connection refused") {
		t.Fatalf("the failure cause is not logged: %s", buf.String())
	}
}

func TestRequestLogCarriesRequestID(t *testing.T) {
	var buf bytes.Buffer
	h := NewRouter(Deps{Log: slog.New(slog.NewJSONHandler(&buf, nil)), DB: fakeDB{}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log line is not JSON: %v (%q)", err, buf.String())
	}
	if line["request_id"] == "" || line["request_id"] == nil {
		t.Errorf("log line has no request_id: %v", line)
	}
	if line["path"] != "/healthz" || line["status"] != float64(200) {
		t.Errorf("unexpected log line: %v", line)
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestRouter(fakeDB{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
