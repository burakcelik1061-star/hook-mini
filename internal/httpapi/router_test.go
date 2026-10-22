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
	"regexp"
	"strings"
	"testing"

	"github.com/burakcelik1061-star/hook-mini/internal/credential"
	"github.com/burakcelik1061-star/hook-mini/internal/service"
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

var requestIDPattern = regexp.MustCompile(`^[A-Z2-7]{26}$`)

// The client's X-Request-Id is input like any other: it must not end up in
// the logs or be echoed back, and two requests must never share an ID.
func TestRequestIDIsAlwaysTheServers(t *testing.T) {
	var buf bytes.Buffer
	h := NewRouter(Deps{Log: slog.New(slog.NewJSONHandler(&buf, nil)), DB: fakeDB{}})

	forged := "victim-request-id\n" + strings.Repeat("A", 4096)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-Id", forged)
	first := httptest.NewRecorder()
	h.ServeHTTP(first, req)
	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	id := first.Header().Get("X-Request-Id")
	if !requestIDPattern.MatchString(id) {
		t.Fatalf("X-Request-Id = %.80q, want a server-generated ID", id)
	}
	if id == second.Header().Get("X-Request-Id") {
		t.Errorf("two requests got the same ID %s", id)
	}
	if strings.Contains(buf.String(), "victim-request-id") {
		t.Error("the client's X-Request-Id reached the log")
	}
	var line map[string]any
	if err := json.Unmarshal(bytes.SplitN(buf.Bytes(), []byte("\n"), 2)[0], &line); err != nil {
		t.Fatal(err)
	}
	if line["request_id"] != id {
		t.Errorf("logged request_id %v, but the response said %s", line["request_id"], id)
	}
}

// A panic must still produce the documented error envelope (not chi's bare
// 500) and a log line that ties the stack to the request ID.
func TestPanicAnswersWithTheErrorEnvelope(t *testing.T) {
	var buf bytes.Buffer
	// A Service without a database: authenticating dereferences nil.
	h := NewRouter(Deps{Log: slog.New(slog.NewJSONHandler(&buf, nil)), DB: fakeDB{}, Svc: &service.Service{}})
	key, err := credential.NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/endpoints", nil)
	req.Header.Set("Authorization", "Bearer "+key.Full)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if err := loadSpec(t).ValidateResponse("GET", "/v1/endpoints", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()); err != nil {
		t.Errorf("the panic response does not match the document: %v (%s)", err, rec.Body)
	}
	id := rec.Header().Get("X-Request-Id")
	var body struct {
		Error struct {
			Code      string `json:"code"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "internal_error" || body.Error.RequestID != id {
		t.Errorf("body = %s (err %v), want internal_error with request_id %s", rec.Body, err, id)
	}
	logs := buf.String()
	if !strings.Contains(logs, "panic while serving a request") || !strings.Contains(logs, id) || !strings.Contains(logs, "goroutine") {
		t.Errorf("the panic is not logged with its request ID and stack: %s", logs)
	}
}

// Once a response has begun, an error envelope appended to it would only
// corrupt it; the panic is logged and the response left as it is.
func TestPanicAfterTheResponseBeganAddsNothing(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// The same order as in NewRouter: the logger's writer wraps the recoverer.
	h := requestLogger(log)(recoverer(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "partial")
		panic("late failure")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "partial" {
		t.Errorf("got %d %q, want the response left as it was", rec.Code, rec.Body)
	}
}

func TestAbortHandlerPanicIsPassedOn(t *testing.T) {
	h := recoverer(slog.New(slog.NewTextHandler(io.Discard, nil)))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler to reach net/http", v)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	t.Error("the abort was swallowed")
}

func TestUnknownRouteIs404(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestRouter(fakeDB{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// With a console, its routes serve it, and everything else is untouched:
// the API, the health checks, and an unknown path is still a JSON 404.
func TestConsoleRoutesDoNotShadowTheAPI(t *testing.T) {
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "console") })
	h := NewRouter(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: fakeDB{}, UI: ui})
	for _, p := range []string{"/", "/endpoints", "/deliveries", "/send", "/sign-in", "/assets/app.js"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Body.String() != "console" {
			t.Errorf("%s: %q, want the console", p, rec.Body.String())
		}
	}
	for p, want := range map[string]int{"/healthz": 200, "/nope": 404, "/v1": 404} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != want || rec.Body.String() == "console" {
			t.Errorf("%s: %d %q, want %d from the API router", p, rec.Code, rec.Body.String(), want)
		}
	}
	// A POST to a console path is not the console's.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/endpoints", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /endpoints: %d, want 405", rec.Code)
	}
}
