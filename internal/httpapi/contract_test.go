package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/hook-mini/api"
	"github.com/burakcelik1061-star/hook-mini/internal/openapicheck"
	"github.com/burakcelik1061-star/hook-mini/internal/service"
)

// Contract tests: api/openapi.yaml is the published description of the API, so
// it must say exactly what the code does. These tests make the document and the
// implementation check each other.

func loadSpec(t *testing.T) *openapicheck.Spec {
	t.Helper()
	s, err := openapicheck.Load(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Every route the router serves is in the document, and every operation in the
// document is served. Needs no database: the router is only inspected.
func TestRoutesAndDocumentDescribeTheSameOperations(t *testing.T) {
	spec := loadSpec(t)
	router := NewRouter(Deps{Log: testLog, DB: fakeDB{}, Svc: service.New(nil, testLog)})

	served := map[string]bool{}
	err := chi.Walk(router.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = strings.TrimSuffix(route, "/")
		if route == "" {
			route = "/"
		}
		served[method+" "+route] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	documented := map[string]bool{}
	for _, op := range spec.Operations() {
		documented[op.String()] = true
	}
	for op := range served {
		if !documented[op] {
			t.Errorf("%s is served but not in api/openapi.yaml", op)
		}
	}
	for op := range documented {
		if !served[op] {
			t.Errorf("%s is in api/openapi.yaml but the router does not serve it", op)
		}
	}
}

func TestOpenAPIIsServed(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestRouter(fakeDB{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/yaml") {
		t.Fatalf("%d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !bytes.Equal(rec.Body.Bytes(), api.OpenAPI) {
		t.Error("the served document differs from the embedded one")
	}
	// It needs no key, like the health checks.
	if err := loadSpec(t).ValidateResponse("GET", "/openapi.yaml", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()); err != nil {
		t.Error(err)
	}
}

func TestReadinessResponsesMatchTheDocument(t *testing.T) {
	spec := loadSpec(t)
	for _, tc := range []struct {
		path   string
		db     Pinger
		status int
	}{
		{"/healthz", fakeDB{}, 200},
		{"/readyz", fakeDB{}, 200},
		{"/readyz", fakeDB{err: errors.New("down")}, 503},
	} {
		rec := httptest.NewRecorder()
		newTestRouter(tc.db).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.status {
			t.Fatalf("%s: %d, want %d", tc.path, rec.Code, tc.status)
		}
		if err := spec.ValidateResponse("GET", tc.path, rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()); err != nil {
			t.Errorf("%s: %v", tc.path, err)
		}
	}
}

// What the document tells readers to send must be accepted. A request example
// that the server rejects is documentation that misleads.
func TestRequestExamplesInTheDocumentAreAccepted(t *testing.T) {
	srv, svc := newServer(t)
	spec := loadSpec(t)
	c := newTenantClient(t, srv, svc)
	example := func(name string) []byte {
		t.Helper()
		b, err := spec.Example(name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	res := c.send("POST", "/v1/endpoints", example("CreateEndpointRequest"))
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("the CreateEndpointRequest example was refused: %d %s", res.StatusCode, res.Body)
	}
	id := res.JSON()["id"].(string)
	if res := c.send("PATCH", "/v1/endpoints/"+id, example("UpdateEndpointRequest")); res.StatusCode != http.StatusOK {
		t.Errorf("the UpdateEndpointRequest example was refused: %d %s", res.StatusCode, res.Body)
	}
	if res := c.send("POST", "/v1/events", example("CreateEventRequest")); res.StatusCode != http.StatusCreated {
		t.Errorf("the CreateEventRequest example was refused: %d %s", res.StatusCode, res.Body)
	}
}

// observed records "METHOD path status" for every response the contract test
// validated, so the test can also prove that nothing the document declares is
// fiction.
type observed map[string]bool

func (o observed) key(method, path string, status int) string {
	return fmt.Sprintf("%s %s %d", method, path, status)
}

// contract is the harness: it sends a request, requires the expected status
// and validates the response against the document.
type contract struct {
	t    *testing.T
	spec *openapicheck.Spec
	seen observed
}

func (k *contract) do(c *tenantClient, method, specPath, realPath string, want int, body any, headers ...string) response {
	k.t.Helper()
	res := c.send(method, realPath, body, headers...)
	k.verify(method, specPath, want, res)
	return res
}

func (k *contract) verify(method, specPath string, want int, res response) {
	k.t.Helper()
	if res.StatusCode != want {
		k.t.Errorf("%s %s: status %d, want %d: %s", method, specPath, res.StatusCode, want, res.Body)
		return
	}
	if err := k.spec.ValidateResponse(method, specPath, res.StatusCode, res.Header.Get("Content-Type"), res.Body); err != nil {
		k.t.Errorf("%s %s %d: %v\n%s", method, specPath, res.StatusCode, err, res.Body)
		return
	}
	k.seen[k.seen.key(method, specPath, res.StatusCode)] = true
}

func TestHTTPResponsesMatchTheDocument(t *testing.T) {
	srv, svc := newServer(t)
	spec := loadSpec(t)
	k := &contract{t: t, spec: spec, seen: observed{}}
	c := newTenantClient(t, srv, svc)
	random := uuid.NewString()

	big := func(n int) string {
		return `{"url":"https://example.invalid","description":"` + strings.Repeat("x", n) + `"}`
	}

	// ---- endpoints ----
	ep := k.do(c, "POST", "/v1/endpoints", "/v1/endpoints", 201, map[string]any{"url": "https://example.invalid/contract", "description": "contract"}).JSON()
	epID := ep["id"].(string)
	k.do(c, "POST", "/v1/endpoints", "/v1/endpoints", 400, map[string]any{"url": "ftp://x"})
	k.do(c, "POST", "/v1/endpoints", "/v1/endpoints", 413, big(20<<10))
	k.do(c, "POST", "/v1/endpoints", "/v1/endpoints", 415, `{"url":"https://example.invalid"}`, "Content-Type: text/plain")

	k.do(c, "GET", "/v1/endpoints", "/v1/endpoints", 200, nil)
	k.do(c, "GET", "/v1/endpoints", "/v1/endpoints?limit=0", 400, nil)
	k.do(c, "GET", "/v1/endpoints/{id}", "/v1/endpoints/"+epID, 200, nil)
	k.do(c, "PATCH", "/v1/endpoints/{id}", "/v1/endpoints/"+epID, 200, map[string]any{"description": "changed"})
	k.do(c, "PATCH", "/v1/endpoints/{id}", "/v1/endpoints/"+epID, 400, map[string]any{})
	k.do(c, "PATCH", "/v1/endpoints/{id}", "/v1/endpoints/"+epID, 413, big(20<<10))
	k.do(c, "PATCH", "/v1/endpoints/{id}", "/v1/endpoints/"+epID, 415, `{"description":"x"}`, "Content-Type: text/plain")
	k.do(c, "POST", "/v1/endpoints/{id}/rotate-secret", "/v1/endpoints/"+epID+"/rotate-secret", 200, nil)
	k.do(c, "POST", "/v1/endpoints/{id}/disable", "/v1/endpoints/"+epID+"/disable", 200, nil)
	k.do(c, "POST", "/v1/endpoints/{id}/enable", "/v1/endpoints/"+epID+"/enable", 200, nil)

	// ---- events ----
	body := map[string]any{"type": "contract.test", "payload": map[string]any{"n": 1}}
	ev := k.do(c, "POST", "/v1/events", "/v1/events", 201, body, "Idempotency-Key: contract-1").JSON()
	evID := ev["id"].(string)
	k.do(c, "POST", "/v1/events", "/v1/events", 200, body, "Idempotency-Key: contract-1")
	k.do(c, "POST", "/v1/events", "/v1/events", 422, map[string]any{"type": "contract.test", "payload": map[string]any{"n": 2}}, "Idempotency-Key: contract-1")
	k.do(c, "POST", "/v1/events", "/v1/events", 400, map[string]any{"type": "Bad Type", "payload": map[string]any{}})
	k.do(c, "POST", "/v1/events", "/v1/events", 413, `{"type":"a.b","payload":{"d":"`+strings.Repeat("x", 300<<10)+`"}}`)
	k.do(c, "POST", "/v1/events", "/v1/events", 415, `{"type":"a.b","payload":{}}`, "Content-Type: text/plain")
	k.do(c, "GET", "/v1/events", "/v1/events", 200, nil)
	k.do(c, "GET", "/v1/events", "/v1/events?cursor=garbage", 400, nil)
	k.do(c, "GET", "/v1/events/{id}", "/v1/events/"+evID, 200, nil)

	// ---- deliveries ----
	list := k.do(c, "GET", "/v1/deliveries", "/v1/deliveries?event_id="+evID, 200, nil).JSON()
	dID := list["data"].([]any)[0].(map[string]any)["id"].(string)
	k.do(c, "GET", "/v1/deliveries", "/v1/deliveries?status=failed", 400, nil)
	k.do(c, "POST", "/v1/deliveries/{id}/replay", "/v1/deliveries/"+dID+"/replay", 409, nil) // still pending
	forceStatus(t, dID, "dead")
	if _, err := testPool.Exec(context.Background(), `INSERT INTO delivery_attempts (delivery_id, attempt_no, started_at, duration_ms, status_code, response_snippet, error)
		VALUES ($1, 1, now(), 12, 503, 'try later', 'endpoint responded with HTTP 503'), ($1, 2, now(), 3, NULL, NULL, 'timeout')`, dID); err != nil {
		t.Fatal(err)
	}
	k.do(c, "GET", "/v1/deliveries", "/v1/deliveries?status=dead", 200, nil)
	detail := k.do(c, "GET", "/v1/deliveries/{id}", "/v1/deliveries/"+dID, 200, nil).JSON()
	if atts, _ := detail["attempts"].([]any); len(atts) != 2 {
		t.Errorf("the detail should carry the two attempts: %v", detail)
	}
	k.do(c, "POST", "/v1/deliveries/{id}/replay", "/v1/deliveries/"+dID+"/replay", 202, nil)

	// ---- every operation, uniformly: no key, unknown id ----
	for _, op := range spec.Operations() {
		if !strings.HasPrefix(op.Path, "/v1/") {
			continue
		}
		real := strings.ReplaceAll(op.Path, "{id}", random)
		k.verify(op.Method, op.Path, 401, rawSend(t, srv, op.Method, real, "", nil))
		if strings.Contains(op.Path, "{id}") {
			var reqBody any
			if op.Method == "PATCH" {
				reqBody = map[string]any{"description": "x"}
			}
			k.do(c, op.Method, op.Path, real, 404, reqBody)
		}
	}
	// Deleting last, so the endpoint stayed available for the calls above.
	k.do(c, "DELETE", "/v1/endpoints/{id}", "/v1/endpoints/"+epID, 204, nil)

	// ---- 500 from every operation: the database is gone ----
	k.fiveHundreds(t)

	// ---- the document declares nothing that never happens ----
	k.seen[k.seen.key("GET", "/healthz", 200)] = true // covered by TestReadinessResponsesMatchTheDocument
	k.seen[k.seen.key("GET", "/readyz", 200)] = true
	k.seen[k.seen.key("GET", "/readyz", 503)] = true
	k.seen[k.seen.key("GET", "/openapi.yaml", 200)] = true // covered by TestOpenAPIIsServed
	var missing []string
	for _, op := range spec.Operations() {
		statuses, _ := spec.Statuses(op.Method, op.Path)
		for _, st := range statuses {
			if !k.seen[op.Method+" "+op.Path+" "+st] {
				missing = append(missing, fmt.Sprintf("%s %s", op, st))
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("api/openapi.yaml declares responses that no test ever produced (documented but never seen):\n  %s", strings.Join(missing, "\n  "))
	}
}

// fiveHundreds sends a request to every /v1 operation on a server whose
// database is gone and checks the 500 each one declares. Authentication is the
// first thing that touches the database, so every operation fails there.
func (k *contract) fiveHundreds(t *testing.T) {
	t.Helper()
	deadPool, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	deadPool.Close()
	srv := httptest.NewServer(NewRouter(Deps{Log: testLog, DB: testPool, Svc: service.New(deadPool, testLog)}))
	defer srv.Close()

	key := "hm_00000000_" + strings.Repeat("a", 64) // well-formed, so it reaches the lookup
	for _, op := range k.spec.Operations() {
		if !strings.HasPrefix(op.Path, "/v1/") {
			continue
		}
		real := strings.ReplaceAll(op.Path, "{id}", uuid.NewString())
		res := rawSend(t, srv, op.Method, real, key, nil)
		k.verify(op.Method, op.Path, 500, res)
		if strings.Contains(string(res.Body), "closed pool") {
			t.Errorf("%s: the 500 leaks an internal message: %s", op, res.Body)
		}
	}
}
