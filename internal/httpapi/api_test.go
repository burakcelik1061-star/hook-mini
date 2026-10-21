package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/hook-mini/internal/database"
	"github.com/burakcelik1061-star/hook-mini/internal/secretbox/secretboxtest"
	"github.com/burakcelik1061-star/hook-mini/internal/service"
)

// End-to-end tests of the /v1 API: a real HTTP server, the real service and a
// real Postgres. Point TEST_DATABASE_URL at a throwaway database (see README);
// without it they skip.

var (
	testPool *pgxpool.Pool
	testLog  = slog.New(slog.NewTextHandler(io.Discard, nil))
)

func TestMain(m *testing.M) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		os.Exit(m.Run())
	}
	ctx := context.Background()
	if _, err := database.Migrate(ctx, testLog, url); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
	pool, err := database.Connect(ctx, testLog, url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	testPool = pool
	code := m.Run()
	pool.Close()
	os.Exit(code)
}

// tenantClient talks to the API as one tenant.
type tenantClient struct {
	t      *testing.T
	srv    *httptest.Server
	svc    *service.Service
	tenant uuid.UUID
	keyID  uuid.UUID
	key    string
}

func newServer(t *testing.T) (*httptest.Server, *service.Service) {
	t.Helper()
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL not set")
	}
	svc := service.New(testPool, testLog, service.WithSecretKeys(secretboxtest.Keyring()))
	srv := httptest.NewServer(NewRouter(Deps{Log: testLog, DB: testPool, Svc: svc}))
	t.Cleanup(srv.Close)
	return srv, svc
}

func newTenantClient(t *testing.T, srv *httptest.Server, svc *service.Service) *tenantClient {
	t.Helper()
	tn, err := svc.CreateTenant(context.Background(), "Test "+uuid.NewString()[:8])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, tn.ID)
	})
	row, key, err := svc.IssueAPIKey(context.Background(), tn.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	return &tenantClient{t: t, srv: srv, svc: svc, tenant: tn.ID, keyID: row.ID, key: key}
}

// response is a fully read HTTP response.
type response struct {
	*http.Response
	Body []byte
}

// JSON decodes the body into a generic map.
func (r response) JSON() map[string]any {
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		panic(fmt.Sprintf("response is not a JSON object: %v\n%s", err, r.Body))
	}
	return m
}

func (r response) errorCode() string {
	e, _ := r.JSON()["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

// send performs a request. body may be a string (sent as is), nil, or any
// value (marshalled to JSON). headers are "Name: value" strings.
func (c *tenantClient) send(method, path string, body any, headers ...string) response {
	c.t.Helper()
	return rawSend(c.t, c.srv, method, path, c.key, body, headers...)
}

func rawSend(t *testing.T, srv *httptest.Server, method, path, key string, body any, headers ...string) response {
	t.Helper()
	var rdr io.Reader
	hasBody := false
	switch b := body.(type) {
	case nil:
	case string:
		rdr, hasBody = strings.NewReader(b), true
	case []byte:
		rdr, hasBody = bytes.NewReader(b), true
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rdr, hasBody = bytes.NewReader(buf), true
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	// A header named here replaces the helper's default of the same name
	// (Authorization, Content-Type); naming it twice sends it twice.
	replaced := map[string]bool{}
	for _, h := range headers {
		name, value, _ := strings.Cut(h, ": ")
		canon := http.CanonicalHeaderKey(name)
		if !replaced[canon] {
			req.Header.Del(canon)
			replaced[canon] = true
		}
		if value != "<delete>" {
			req.Header.Add(canon, value)
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return response{Response: res, Body: data}
}

func (c *tenantClient) createEndpoint(url string) map[string]any {
	c.t.Helper()
	res := c.send("POST", "/v1/endpoints", map[string]any{"url": url})
	if res.StatusCode != http.StatusCreated {
		c.t.Fatalf("create endpoint: %d %s", res.StatusCode, res.Body)
	}
	return res.JSON()
}

func TestHealthEndpointsNeedNoAuth(t *testing.T) {
	srv, _ := newServer(t)
	for _, p := range []string{"/healthz", "/readyz"} {
		if res := rawSend(t, srv, "GET", p, "", nil); res.StatusCode != http.StatusOK {
			t.Errorf("%s without a key: %d, want 200", p, res.StatusCode)
		}
	}
}

func TestAuthentication(t *testing.T) {
	srv, svc := newServer(t)
	c := newTenantClient(t, srv, svc)

	good := c.key
	wrongSecret := good[:len(good)-1] + map[bool]string{true: "0", false: "1"}[good[len(good)-1] != '0']

	cases := map[string][]string{
		"no header":       {"Authorization: <delete>"},
		"basic scheme":    {"Authorization: Basic " + good},
		"empty bearer":    {"Authorization: Bearer "},
		"malformed key":   {"Authorization: Bearer hm_nonsense"},
		"wrong secret":    {"Authorization: Bearer " + wrongSecret},
		"unknown prefix":  {"Authorization: Bearer hm_00000000_" + strings.Repeat("a", 64)},
		"key as raw text": {"Authorization: " + good},
	}
	var firstBody string
	for name, hdr := range cases {
		res := c.send("GET", "/v1/endpoints", nil, hdr...)
		if res.StatusCode != http.StatusUnauthorized || res.errorCode() != "unauthorized" {
			t.Errorf("%s: %d %s, want 401 unauthorized", name, res.StatusCode, res.Body)
			continue
		}
		if !strings.HasPrefix(res.Header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("%s: missing WWW-Authenticate challenge", name)
		}
		// The client must not learn why a key was rejected: apart from the
		// per-request ID, every failure returns the same body.
		body := strings.Join(strings.Fields(removeRequestID(res.Body)), " ")
		if firstBody == "" {
			firstBody = body
		} else if body != firstBody {
			t.Errorf("%s: response differs from the others, which leaks the reason:\n%s\nvs\n%s", name, body, firstBody)
		}
	}

	if res := c.send("GET", "/v1/endpoints", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("a valid key was rejected: %d %s", res.StatusCode, res.Body)
	}

	if err := c.svc.RevokeAPIKey(context.Background(), c.tenant, c.keyID); err != nil {
		t.Fatal(err)
	}
	if res := c.send("GET", "/v1/endpoints", nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a revoked key still works: %d", res.StatusCode)
	}
}

func removeRequestID(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return string(body)
	}
	if e, ok := m["error"].(map[string]any); ok {
		delete(e, "request_id")
	}
	out, _ := json.Marshal(m)
	return string(out)
}

func TestEndpointLifecycleOverHTTP(t *testing.T) {
	srv, svc := newServer(t)
	c := newTenantClient(t, srv, svc)

	res := c.send("POST", "/v1/endpoints", map[string]any{"url": "https://example.invalid/hook", "description": "billing"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", res.StatusCode, res.Body)
	}
	created := res.JSON()
	id, _ := created["id"].(string)
	if loc := res.Header.Get("Location"); loc != "/v1/endpoints/"+id {
		t.Errorf("Location = %q", loc)
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q: a response with a secret must not be cacheable", res.Header.Get("Cache-Control"))
	}
	if res.Header.Get("X-Request-Id") == "" {
		t.Error("no X-Request-Id header")
	}
	secret, _ := created["secret"].(string)
	if !strings.HasPrefix(secret, "whsec_") {
		t.Fatalf("creation response has no signing secret: %s", res.Body)
	}
	if created["status"] != "active" || created["description"] != "billing" || created["url"] != "https://example.invalid/hook" {
		t.Errorf("unexpected body: %s", res.Body)
	}

	// The secret is shown once: no later response contains it, anywhere.
	for name, r := range map[string]response{
		"get":    c.send("GET", "/v1/endpoints/"+id, nil),
		"list":   c.send("GET", "/v1/endpoints", nil),
		"patch":  c.send("PATCH", "/v1/endpoints/"+id, map[string]any{"description": "updated"}),
		"events": c.send("GET", "/v1/events", nil),
	} {
		if r.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", name, r.StatusCode, r.Body)
		}
		if strings.Contains(string(r.Body), secret) || strings.Contains(string(r.Body), `"secret"`) {
			t.Errorf("%s response exposes the signing secret: %s", name, r.Body)
		}
	}

	got := c.send("GET", "/v1/endpoints/"+id, nil).JSON()
	if got["description"] != "updated" || got["url"] != "https://example.invalid/hook" {
		t.Errorf("patch did not apply or changed other fields: %v", got)
	}

	if res := c.send("DELETE", "/v1/endpoints/"+id, nil); res.StatusCode != http.StatusNoContent || len(res.Body) != 0 {
		t.Errorf("delete: %d %q, want 204 with no body", res.StatusCode, res.Body)
	}
	gone := c.send("GET", "/v1/endpoints/"+id, nil)
	if gone.StatusCode != http.StatusNotFound || gone.errorCode() != "not_found" {
		t.Errorf("after delete: %d %s", gone.StatusCode, gone.Body)
	}
	if e, _ := gone.JSON()["error"].(map[string]any); e["request_id"] == "" || e["request_id"] == nil {
		t.Errorf("error responses must carry the request id: %s", gone.Body)
	}
}

// forceStatus puts a delivery in a given state (no worker runs in these tests).
func forceStatus(t *testing.T, id, status string) {
	t.Helper()
	q := map[string]string{
		"dead":      `UPDATE deliveries SET status = 'dead', attempt_count = 2, completed_at = now(), last_status_code = 503, last_error = 'endpoint responded with HTTP 503' WHERE id = $1`,
		"succeeded": `UPDATE deliveries SET status = 'succeeded', attempt_count = 1, completed_at = now(), last_status_code = 200 WHERE id = $1`,
	}[status]
	if _, err := testPool.Exec(context.Background(), q, id); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryHistoryAndReplayOverHTTP(t *testing.T) {
	srv, svc := newServer(t)
	a, b := newTenantClient(t, srv, svc), newTenantClient(t, srv, svc)
	ep := a.createEndpoint("https://example.invalid/history")
	epID := ep["id"].(string)
	ev := a.send("POST", "/v1/events", map[string]any{"type": "a.b", "payload": map[string]any{"n": 1}}).JSON()
	evID := ev["id"].(string)

	list := a.send("GET", "/v1/deliveries", nil)
	items := list.JSON()["data"].([]any)
	if list.StatusCode != 200 || len(items) != 1 {
		t.Fatalf("list: %d %s", list.StatusCode, list.Body)
	}
	d := items[0].(map[string]any)
	id := d["id"].(string)
	if d["status"] != "pending" || d["endpoint_id"] != epID || d["event_id"] != evID || d["next_attempt_at"] == nil {
		t.Errorf("a new delivery: %v", d)
	}
	if _, has := d["attempts"]; has {
		t.Error("lists must not embed attempts")
	}

	// Detail carries the attempts; a pending delivery cannot be replayed.
	if r := a.send("POST", "/v1/deliveries/"+id+"/replay", nil); r.StatusCode != 409 || r.errorCode() != "delivery_not_replayable" {
		t.Errorf("replay of a pending delivery: %d %s", r.StatusCode, r.Body)
	}

	forceStatus(t, id, "dead")
	if _, err := testPool.Exec(context.Background(), `INSERT INTO delivery_attempts (delivery_id, attempt_no, started_at, duration_ms, status_code, response_snippet, error)
		VALUES ($1, 1, now(), 12, 503, 'try later', 'endpoint responded with HTTP 503'), ($1, 2, now(), 9, 503, NULL, 'endpoint responded with HTTP 503')`, id); err != nil {
		t.Fatal(err)
	}
	detail := a.send("GET", "/v1/deliveries/"+id, nil).JSON()
	atts, _ := detail["attempts"].([]any)
	if detail["status"] != "dead" || len(atts) != 2 || detail["next_attempt_at"] != nil || detail["completed_at"] == nil {
		t.Fatalf("detail: %v", detail)
	}
	if first := atts[0].(map[string]any); first["attempt_no"] != float64(1) || first["status_code"] != float64(503) || first["response_snippet"] != "try later" {
		t.Errorf("first attempt: %v", first)
	}

	// Filters.
	for name, tc := range map[string]struct {
		query string
		want  int
	}{
		"status dead":       {"?status=dead", 1},
		"status pending":    {"?status=pending", 0},
		"by endpoint":       {"?endpoint_id=" + epID, 1},
		"by event":          {"?event_id=" + evID, 1},
		"other endpoint id": {"?endpoint_id=" + uuid.NewString(), 0},
		"combined":          {"?status=dead&endpoint_id=" + epID + "&event_id=" + evID, 1},
	} {
		res := a.send("GET", "/v1/deliveries"+tc.query, nil)
		if got := len(res.JSON()["data"].([]any)); res.StatusCode != 200 || got != tc.want {
			t.Errorf("%s: %d, %d items, want %d", name, res.StatusCode, got, tc.want)
		}
	}
	for name, q := range map[string]string{"bad status": "?status=failed", "bad endpoint id": "?endpoint_id=nope", "bad event id": "?event_id=1"} {
		if r := a.send("GET", "/v1/deliveries"+q, nil); r.StatusCode != 400 || r.errorCode() != "invalid_request" {
			t.Errorf("%s: %d %s, want 400", name, r.StatusCode, r.Body)
		}
	}

	// Replay: 202 and pending again; a second replay is refused.
	r := a.send("POST", "/v1/deliveries/"+id+"/replay", nil)
	if r.StatusCode != http.StatusAccepted || r.JSON()["status"] != "pending" || r.JSON()["attempt_count"] != float64(2) {
		t.Fatalf("replay: %d %s", r.StatusCode, r.Body)
	}
	if r := a.send("POST", "/v1/deliveries/"+id+"/replay", nil); r.StatusCode != 409 || r.errorCode() != "delivery_not_replayable" {
		t.Errorf("second replay: %d %s", r.StatusCode, r.Body)
	}

	// Isolation: another tenant sees and touches nothing.
	if r := b.send("GET", "/v1/deliveries/"+id, nil); r.StatusCode != 404 {
		t.Errorf("another tenant reads a delivery: %d", r.StatusCode)
	}
	forceStatus(t, id, "dead")
	if r := b.send("POST", "/v1/deliveries/"+id+"/replay", nil); r.StatusCode != 404 {
		t.Errorf("another tenant replays: %d", r.StatusCode)
	}
	if got := len(b.send("GET", "/v1/deliveries", nil).JSON()["data"].([]any)); got != 0 {
		t.Errorf("another tenant lists %d deliveries", got)
	}
	if r := a.send("GET", "/v1/deliveries/not-a-uuid", nil); r.StatusCode != 404 {
		t.Errorf("malformed id: %d, want 404", r.StatusCode)
	}
	if r := a.send("DELETE", "/v1/deliveries/"+id, nil); r.StatusCode != 405 {
		t.Errorf("DELETE on a delivery: %d, want 405", r.StatusCode)
	}
}

func TestDisableEnableAndReplayRulesOverHTTP(t *testing.T) {
	srv, svc := newServer(t)
	a, b := newTenantClient(t, srv, svc), newTenantClient(t, srv, svc)
	epID := a.createEndpoint("https://example.invalid/toggle")["id"].(string)
	a.send("POST", "/v1/events", map[string]any{"type": "a.b", "payload": map[string]any{}})
	id := a.send("GET", "/v1/deliveries", nil).JSON()["data"].([]any)[0].(map[string]any)["id"].(string)
	forceStatus(t, id, "dead")

	off := a.send("POST", "/v1/endpoints/"+epID+"/disable", nil)
	if off.StatusCode != 200 || off.JSON()["status"] != "disabled" || off.JSON()["disabled_reason"] != "disabled manually" || strings.Contains(string(off.Body), "whsec_") {
		t.Fatalf("disable: %d %s", off.StatusCode, off.Body)
	}
	if r := a.send("POST", "/v1/endpoints/"+epID+"/disable", nil); r.StatusCode != 200 {
		t.Errorf("disabling twice: %d, want 200 (idempotent)", r.StatusCode)
	}

	// A new event no longer fans out to it, and replay is refused with a
	// reason that tells the caller what to do.
	if n := a.send("POST", "/v1/events", map[string]any{"type": "a.b", "payload": map[string]any{}}).JSON()["delivery_count"]; n != float64(0) {
		t.Errorf("delivery_count = %v for a disabled endpoint, want 0", n)
	}
	if r := a.send("POST", "/v1/deliveries/"+id+"/replay", nil); r.StatusCode != 409 || r.errorCode() != "endpoint_disabled" {
		t.Errorf("replay to a disabled endpoint: %d %s", r.StatusCode, r.Body)
	}

	on := a.send("POST", "/v1/endpoints/"+epID+"/enable", nil)
	if on.StatusCode != 200 || on.JSON()["status"] != "active" || on.JSON()["consecutive_failures"] != float64(0) || on.JSON()["disabled_at"] != nil {
		t.Fatalf("enable: %d %s", on.StatusCode, on.Body)
	}
	if r := a.send("POST", "/v1/deliveries/"+id+"/replay", nil); r.StatusCode != http.StatusAccepted {
		t.Errorf("replay after enabling: %d %s", r.StatusCode, r.Body)
	}

	for name, r := range map[string]response{
		"disable": b.send("POST", "/v1/endpoints/"+epID+"/disable", nil),
		"enable":  b.send("POST", "/v1/endpoints/"+epID+"/enable", nil),
	} {
		if r.StatusCode != 404 {
			t.Errorf("%s by another tenant: %d, want 404", name, r.StatusCode)
		}
	}
	if r := a.send("GET", "/v1/endpoints/"+epID+"/disable", nil); r.StatusCode != 405 {
		t.Errorf("GET on disable: %d, want 405", r.StatusCode)
	}
}

func TestSecretRotationOverHTTP(t *testing.T) {
	srv, svc := newServer(t)
	a, b := newTenantClient(t, srv, svc), newTenantClient(t, srv, svc)
	created := a.createEndpoint("https://example.invalid/rotate")
	id, oldSecret := created["id"].(string), created["secret"].(string)

	res := a.send("POST", "/v1/endpoints/"+id+"/rotate-secret", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("rotate: %d %s", res.StatusCode, res.Body)
	}
	body := res.JSON()
	newSecret, _ := body["secret"].(string)
	if !strings.HasPrefix(newSecret, "whsec_") || newSecret == oldSecret {
		t.Errorf("rotation did not return a fresh secret: %s", res.Body)
	}
	if _, ok := body["previous_secret_expires_at"]; !ok {
		t.Errorf("rotation response lacks previous_secret_expires_at: %s", res.Body)
	}
	if strings.Contains(string(res.Body), oldSecret) {
		t.Error("the rotation response contains the previous secret")
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Error("a response with a secret must not be cacheable")
	}

	got := a.send("GET", "/v1/endpoints/"+id, nil)
	if strings.Contains(string(got.Body), "whsec_") || !strings.Contains(string(got.Body), "previous_secret_expires_at") {
		t.Errorf("GET after rotation: %s", got.Body)
	}

	if r := b.send("POST", "/v1/endpoints/"+id+"/rotate-secret", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("rotation by another tenant: %d, want 404", r.StatusCode)
	}
	if r := a.send("GET", "/v1/endpoints/"+id+"/rotate-secret", nil); r.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET on rotate-secret: %d, want 405", r.StatusCode)
	}
}

func TestPrivateDestinationsAreRejectedOverHTTP(t *testing.T) {
	srv, svc := newServer(t)
	c := newTenantClient(t, srv, svc)
	for _, u := range []string{"http://127.0.0.1:5432/", "http://localhost/", "http://169.254.169.254/latest/meta-data/", "http://[::1]:8080/", "http://10.1.2.3/"} {
		res := c.send("POST", "/v1/endpoints", map[string]any{"url": u})
		if res.StatusCode != http.StatusBadRequest || res.errorCode() != "invalid_request" {
			t.Errorf("%s: %d %s, want 400", u, res.StatusCode, res.Body)
			continue
		}
		if e := res.JSON()["error"].(map[string]any); e["field"] != "url" || !strings.Contains(e["message"].(string), "not allowed") {
			t.Errorf("%s: %s", u, res.Body)
		}
	}
}

func TestTenantsAreIsolatedOverHTTP(t *testing.T) {
	srv, svc := newServer(t)
	a, b := newTenantClient(t, srv, svc), newTenantClient(t, srv, svc)

	ep := a.createEndpoint("https://example.invalid/a")
	epID := ep["id"].(string)
	evRes := a.send("POST", "/v1/events", map[string]any{"type": "a.created", "payload": map[string]any{"x": 1}})
	if evRes.StatusCode != http.StatusCreated {
		t.Fatalf("%d %s", evRes.StatusCode, evRes.Body)
	}
	evID := evRes.JSON()["id"].(string)

	for name, res := range map[string]response{
		"get endpoint":    b.send("GET", "/v1/endpoints/"+epID, nil),
		"patch endpoint":  b.send("PATCH", "/v1/endpoints/"+epID, map[string]any{"url": "https://evil.example"}),
		"delete endpoint": b.send("DELETE", "/v1/endpoints/"+epID, nil),
		"get event":       b.send("GET", "/v1/events/"+evID, nil),
	} {
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s by another tenant: %d %s, want 404", name, res.StatusCode, res.Body)
		}
	}
	for name, path := range map[string]string{"endpoints": "/v1/endpoints", "events": "/v1/events"} {
		res := b.send("GET", path, nil)
		data, _ := res.JSON()["data"].([]any)
		if res.StatusCode != http.StatusOK || len(data) != 0 {
			t.Errorf("another tenant lists %s: %d %s", name, res.StatusCode, res.Body)
		}
	}

	// A's data is untouched by B's attempts.
	got := a.send("GET", "/v1/endpoints/"+epID, nil).JSON()
	if got["url"] != "https://example.invalid/a" {
		t.Errorf("endpoint was modified by another tenant: %v", got)
	}
}

func TestRequestValidationOverHTTP(t *testing.T) {
	srv, svc := newServer(t)
	c := newTenantClient(t, srv, svc)

	big := func(n int) string {
		return `{"url":"https://example.invalid","description":"` + strings.Repeat("x", n) + `"}`
	}
	bigEvent := `{"type":"a.b","payload":{"d":"` + strings.Repeat("x", 300<<10) + `"}}`

	tests := []struct {
		name    string
		method  string
		path    string
		body    any
		headers []string
		status  int
		code    string
		field   string
	}{
		{"wrong content type", "POST", "/v1/endpoints", `{"url":"https://e.invalid"}`, []string{"Content-Type: text/plain"}, 415, "unsupported_media_type", ""},
		{"no content type", "POST", "/v1/endpoints", `{"url":"https://e.invalid"}`, []string{"Content-Type: <delete>"}, 415, "unsupported_media_type", ""},
		{"empty body", "POST", "/v1/endpoints", "", nil, 400, "invalid_request", "body"},
		{"not json", "POST", "/v1/endpoints", "{nope", nil, 400, "invalid_request", "body"},
		{"truncated json", "POST", "/v1/endpoints", `{"url":`, nil, 400, "invalid_request", "body"},
		{"unknown field", "POST", "/v1/endpoints", `{"url":"https://e.invalid","descripton":"typo"}`, nil, 400, "invalid_request", "descripton"},
		{"wrong type", "POST", "/v1/endpoints", `{"url":123}`, nil, 400, "invalid_request", "url"},
		{"two json values", "POST", "/v1/endpoints", `{"url":"https://e.invalid"}{"url":"https://f.invalid"}`, nil, 400, "invalid_request", "body"},
		{"trailing garbage", "POST", "/v1/endpoints", `{"url":"https://e.invalid"} x`, nil, 400, "invalid_request", "body"},
		{"missing url", "POST", "/v1/endpoints", `{}`, nil, 400, "invalid_request", "url"},
		{"bad url scheme", "POST", "/v1/endpoints", `{"url":"ftp://e.com"}`, nil, 400, "invalid_request", "url"},
		{"url with credentials", "POST", "/v1/endpoints", `{"url":"https://u:p@e.com"}`, nil, 400, "invalid_request", "url"},
		{"description too long", "POST", "/v1/endpoints", big(501), nil, 400, "invalid_request", "description"},
		{"endpoint body too large", "POST", "/v1/endpoints", big(20 << 10), nil, 413, "payload_too_large", ""},
		{"empty patch", "PATCH", "/v1/endpoints/" + uuid.NewString(), `{}`, nil, 400, "invalid_request", "body"},
		{"event body too large", "POST", "/v1/events", bigEvent, nil, 413, "payload_too_large", ""},
		{"event without type", "POST", "/v1/events", `{"payload":{}}`, nil, 400, "invalid_request", "type"},
		{"event bad type", "POST", "/v1/events", `{"type":"Bad Type","payload":{}}`, nil, 400, "invalid_request", "type"},
		{"event without payload", "POST", "/v1/events", `{"type":"a.b"}`, nil, 400, "invalid_request", "payload"},
		{"event array payload", "POST", "/v1/events", `{"type":"a.b","payload":[1]}`, nil, 400, "invalid_request", "payload"},
		{"event null payload", "POST", "/v1/events", `{"type":"a.b","payload":null}`, nil, 400, "invalid_request", "payload"},
		{"two idempotency headers", "POST", "/v1/events", `{"type":"a.b","payload":{}}`, []string{"Idempotency-Key: a", "Idempotency-Key: b"}, 400, "invalid_request", "Idempotency-Key"},
		{"empty idempotency header", "POST", "/v1/events", `{"type":"a.b","payload":{}}`, []string{"Idempotency-Key: "}, 400, "invalid_request", "Idempotency-Key"},
		{"limit not a number", "GET", "/v1/endpoints?limit=abc", nil, nil, 400, "invalid_request", "limit"},
		{"limit zero", "GET", "/v1/events?limit=0", nil, nil, 400, "invalid_request", "limit"},
		{"limit too big", "GET", "/v1/events?limit=101", nil, nil, 400, "invalid_request", "limit"},
		{"bad cursor", "GET", "/v1/events?cursor=garbage", nil, nil, 400, "invalid_request", "cursor"},
		{"malformed id", "GET", "/v1/endpoints/not-a-uuid", nil, nil, 404, "not_found", ""},
		{"unknown route", "GET", "/v1/nope", nil, nil, 404, "not_found", ""},
		{"wrong method", "PUT", "/v1/endpoints", `{}`, nil, 405, "method_not_allowed", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := c.send(tt.method, tt.path, tt.body, tt.headers...)
			if res.StatusCode != tt.status || res.errorCode() != tt.code {
				t.Fatalf("%d %s, want %d %s", res.StatusCode, res.Body, tt.status, tt.code)
			}
			if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("error Content-Type = %q, want JSON", ct)
			}
			e := res.JSON()["error"].(map[string]any)
			if tt.field != "" && e["field"] != tt.field {
				t.Errorf("error field = %v, want %q (%s)", e["field"], tt.field, res.Body)
			}
			if msg, _ := e["message"].(string); msg == "" {
				t.Error("error has no message")
			}
		})
	}

	// None of the rejected requests may have created anything.
	for _, p := range []string{"/v1/endpoints", "/v1/events"} {
		if data := c.send("GET", p, nil).JSON()["data"].([]any); len(data) != 0 {
			t.Errorf("rejected requests created %d rows in %s", len(data), p)
		}
	}
}

func TestEventIngestionStatuses(t *testing.T) {
	srv, svc := newServer(t)
	c := newTenantClient(t, srv, svc)
	c.createEndpoint("https://example.invalid/1")
	c.createEndpoint("https://example.invalid/2")

	body := `{"type":"invoice.paid","payload":{"amount":42,"currency":"EUR"}}`
	first := c.send("POST", "/v1/events", body, "Idempotency-Key: order-1001")
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first: %d %s", first.StatusCode, first.Body)
	}
	f := first.JSON()
	if f["delivery_count"] != float64(2) || f["idempotency_key"] != "order-1001" {
		t.Errorf("first body: %s", first.Body)
	}
	if first.Header.Get("Idempotent-Replayed") != "" {
		t.Error("a new event must not be marked as replayed")
	}
	if loc := first.Header.Get("Location"); loc != "/v1/events/"+f["id"].(string) {
		t.Errorf("Location = %q", loc)
	}

	// Same key and content (reordered, re-spaced): 200, the original event.
	replay := c.send("POST", "/v1/events",
		`{ "payload": {"currency":"EUR", "amount":42}, "type": "invoice.paid" }`, "Idempotency-Key: order-1001")
	if replay.StatusCode != http.StatusOK || replay.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %s (replayed header %q)", replay.StatusCode, replay.Body, replay.Header.Get("Idempotent-Replayed"))
	}
	if r := replay.JSON(); r["id"] != f["id"] || r["delivery_count"] != float64(2) {
		t.Errorf("replay returned a different event: %s", replay.Body)
	}

	// Same key, different content: 422 with a stable error code.
	clash := c.send("POST", "/v1/events", `{"type":"invoice.paid","payload":{"amount":99}}`, "Idempotency-Key: order-1001")
	if clash.StatusCode != http.StatusUnprocessableEntity || clash.errorCode() != "idempotency_key_reuse" {
		t.Errorf("clash: %d %s", clash.StatusCode, clash.Body)
	}

	// Without a key every request is a new event.
	a := c.send("POST", "/v1/events", body)
	b := c.send("POST", "/v1/events", body)
	if a.StatusCode != 201 || b.StatusCode != 201 || a.JSON()["id"] == b.JSON()["id"] {
		t.Errorf("keyless requests: %d %d, ids %v %v", a.StatusCode, b.StatusCode, a.JSON()["id"], b.JSON()["id"])
	}
	if _, has := a.JSON()["idempotency_key"]; has {
		t.Error("idempotency_key must be omitted when none was sent")
	}

	// GET returns the event with its delivery count.
	got := c.send("GET", "/v1/events/"+f["id"].(string), nil).JSON()
	if got["delivery_count"] != float64(2) || got["type"] != "invoice.paid" {
		t.Errorf("get event: %v", got)
	}
}

func TestPayloadSurvivesIntact(t *testing.T) {
	srv, svc := newServer(t)
	c := newTenantClient(t, srv, svc)

	// A 20-digit number must not be rounded through a float64, and non-ASCII
	// text must come back unchanged.
	res := c.send("POST", "/v1/events", `{"type":"data.sync","payload":{"big":12345678901234567890,"name":"Çelik Öğrenci","list":[1,2.50,null,true]}}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("%d %s", res.StatusCode, res.Body)
	}
	got := c.send("GET", "/v1/events/"+res.JSON()["id"].(string), nil)
	for _, want := range []string{`12345678901234567890`, `Çelik Öğrenci`, `2.50`, `null`, `true`} {
		if !strings.Contains(string(got.Body), want) {
			t.Errorf("stored payload lost %q: %s", want, got.Body)
		}
	}
}

func TestListResponses(t *testing.T) {
	srv, svc := newServer(t)
	c := newTenantClient(t, srv, svc)

	// An empty list is [] and next_cursor is null, never missing or "null" data.
	empty := c.send("GET", "/v1/endpoints", nil)
	if string(bytes.TrimSpace(empty.Body)) != `{"data":[],"next_cursor":null}` {
		t.Errorf("empty list = %s", empty.Body)
	}

	for i := range 5 {
		c.createEndpoint(fmt.Sprintf("https://example.invalid/%d", i))
	}
	seen := map[string]bool{}
	path := "/v1/endpoints?limit=2"
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("pagination does not terminate")
		}
		res := c.send("GET", path, nil)
		m := res.JSON()
		for _, it := range m["data"].([]any) {
			id := it.(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatalf("%s appears on two pages", id)
			}
			seen[id] = true
		}
		next, _ := m["next_cursor"].(string)
		if next == "" {
			break
		}
		path = "/v1/endpoints?limit=2&cursor=" + next
	}
	if len(seen) != 5 {
		t.Errorf("walked %d endpoints over HTTP, want 5", len(seen))
	}
}

func TestInternalErrorsDoNotLeak(t *testing.T) {
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL not set")
	}
	// A service whose database is gone: every query fails with a driver error
	// that contains connection details.
	dead, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	dead.Close()
	srv := httptest.NewServer(NewRouter(Deps{Log: testLog, DB: testPool, Svc: service.New(dead, testLog)}))
	defer srv.Close()

	// Any well-formed key reaches the database lookup, which fails.
	key := "hm_00000000_" + strings.Repeat("a", 64)
	res := rawSend(t, srv, "GET", "/v1/endpoints", key, nil)
	if res.StatusCode != http.StatusInternalServerError || res.errorCode() != "internal_error" {
		t.Fatalf("%d %s, want 500 internal_error (a database failure is not a 401)", res.StatusCode, res.Body)
	}
	for _, leak := range []string{"pool", "closed", "pgx", "127.0.0.1", "hookmini", "sql", "dial"} {
		if strings.Contains(strings.ToLower(string(res.Body)), leak) {
			t.Errorf("a 500 response leaks internals (%q): %s", leak, res.Body)
		}
	}
}
