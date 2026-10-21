package delivery

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/hook-mini/api"
	"github.com/burakcelik1061-star/hook-mini/internal/openapicheck"
	"github.com/burakcelik1061-star/hook-mini/internal/safedial"
	"github.com/burakcelik1061-star/hook-mini/pkg/webhook"
)

// testSecret is the Standard Webhooks specification's example secret.
const testSecret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"

// allowAll is a transport that may reach the tests' loopback servers. The
// production default refuses them (see TestDefaultSenderRefusesLoopback).
func allowAll() *http.Transport { return safedial.Policy{AllowPrivate: true}.Transport() }

func testMessage(t *testing.T, url string) (message, uuid.UUID) {
	t.Helper()
	id := uuid.New()
	m, err := newMessage(url, id, "invoice.paid", time.Date(2026, 10, 21, 12, 0, 0, 123000000, time.FixedZone("TRT", 3*3600)), json.RawMessage(`{"amount":42}`), testSecret)
	if err != nil {
		t.Fatal(err)
	}
	return m, id
}

func TestSendRequestShape(t *testing.T) {
	var got *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		body, _ = io.ReadAll(r.Body)
		io.WriteString(w, "thanks")
	}))
	defer srv.Close()

	m, id := testMessage(t, srv.URL+"/hooks?x=1")
	res := newSender(allowAll(), time.Second).send(context.Background(), m)

	if !res.ok() || res.statusCode != 200 || res.snippet != "thanks" || res.errText != "" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if got.Method != http.MethodPost || got.URL.Path != "/hooks" || got.URL.RawQuery != "x=1" {
		t.Errorf("request line: %s %s", got.Method, got.URL)
	}
	for h, want := range map[string]string{
		"Content-Type": "application/json",
		"User-Agent":   userAgent,
		"Webhook-Id":   id.String(),
	} {
		if v := got.Header.Get(h); v != want {
			t.Errorf("%s = %q, want %q", h, v, want)
		}
	}

	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body is not JSON: %s", body)
	}
	if env["id"] != id.String() || env["type"] != "invoice.paid" {
		t.Errorf("envelope: %s", body)
	}
	if data, _ := env["data"].(map[string]any); data["amount"] != float64(42) {
		t.Errorf("payload not carried as data: %s", body)
	}
	// Timestamps are sent in UTC whatever the server's zone.
	if env["created_at"] != "2026-10-21T09:00:00.123Z" {
		t.Errorf("created_at = %v, want UTC 2026-10-21T09:00:00.123Z", env["created_at"])
	}
}

// captureServer records the last request's headers and body.
func captureServer(t *testing.T) (*httptest.Server, func() (http.Header, []byte)) {
	t.Helper()
	var (
		mu   sync.Mutex
		hdr  http.Header
		body []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		hdr, body = r.Header.Clone(), b
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	return srv, func() (http.Header, []byte) {
		mu.Lock()
		defer mu.Unlock()
		return hdr, body
	}
}

func TestRequestsAreSignedPerStandardWebhooks(t *testing.T) {
	srv, last := captureServer(t)
	m, id := testMessage(t, srv.URL)
	if res := newSender(allowAll(), time.Second).send(context.Background(), m); !res.ok() {
		t.Fatalf("send: %+v", res)
	}
	h, body := last()

	if h.Get("Webhook-Id") != id.String() {
		t.Errorf("webhook-id = %q, want the event id", h.Get("Webhook-Id"))
	}
	ts, err := strconv.ParseInt(h.Get("Webhook-Timestamp"), 10, 64)
	if err != nil || time.Since(time.Unix(ts, 0)).Abs() > 5*time.Second {
		t.Errorf("webhook-timestamp = %q, want the current Unix time", h.Get("Webhook-Timestamp"))
	}
	if sig := h.Get("Webhook-Signature"); !strings.HasPrefix(sig, "v1,") || len(strings.Fields(sig)) != 1 {
		t.Errorf("webhook-signature = %q, want one v1 signature", sig)
	}

	// What we send is what api/openapi.yaml documents for the webhook.
	spec, err := openapicheck.Load(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	if err := spec.ValidateWebhook("event.delivery", h, body); err != nil {
		t.Errorf("the request does not match the webhook in api/openapi.yaml: %v", err)
	}

	// The receiver side of the standard accepts it...
	v, _ := webhook.NewVerifier(testSecret)
	if err := v.Verify(h, body); err != nil {
		t.Fatalf("a receiver with the secret rejects the request: %v", err)
	}
	// ...and a receiver with another secret does not.
	other, _ := webhook.NewVerifier("whsec_" + strings.Repeat("Q", 32))
	if err := other.Verify(h, body); err == nil {
		t.Error("a receiver with the wrong secret accepted the request")
	}
}

func TestRotationSignsWithBothSecrets(t *testing.T) {
	srv, last := captureServer(t)
	newSecret := "whsec_" + "bmV3LXNlY3JldC1mb3ItdGhlLXJvdGF0aW9uLXRlc3Q="
	m, err := newMessage(srv.URL, uuid.New(), "a.b", time.Now(), json.RawMessage(`{}`), newSecret, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	newSender(allowAll(), time.Second).send(context.Background(), m)
	h, body := last()

	if n := len(strings.Fields(h.Get("Webhook-Signature"))); n != 2 {
		t.Fatalf("%d signatures, want 2 during a rotation", n)
	}
	for _, secret := range []string{newSecret, testSecret} {
		v, _ := webhook.NewVerifier(secret)
		if err := v.Verify(h, body); err != nil {
			t.Errorf("a receiver with %s... rejects the rotated request: %v", secret[:10], err)
		}
	}
}

func TestEachAttemptIsSignedAtItsOwnTime(t *testing.T) {
	srv, last := captureServer(t)
	m, _ := testMessage(t, srv.URL)
	s := newSender(allowAll(), time.Second)

	clock := time.Unix(1_800_000_000, 0)
	s.now = func() time.Time { return clock }
	s.send(context.Background(), m)
	h1, _ := last()

	clock = clock.Add(3 * time.Hour) // a retry much later
	s.send(context.Background(), m)
	h2, _ := last()

	if h1.Get("Webhook-Timestamp") == h2.Get("Webhook-Timestamp") || h1.Get("Webhook-Signature") == h2.Get("Webhook-Signature") {
		t.Error("a retry reused the first attempt's timestamp and signature; receivers would reject it as a replay")
	}
	if h1.Get("Webhook-Id") != h2.Get("Webhook-Id") {
		t.Error("webhook-id changed between retries; receivers could not deduplicate")
	}
}

func TestNewMessageRejectsBadSecrets(t *testing.T) {
	if _, err := newMessage("http://x.invalid", uuid.New(), "a.b", time.Now(), json.RawMessage(`{}`)); err == nil {
		t.Error("a message without a signing secret was accepted")
	}
	_, err := newMessage("http://x.invalid", uuid.New(), "a.b", time.Now(), json.RawMessage(`{}`), "whsec_not*base64")
	if err == nil {
		t.Fatal("a corrupt secret was accepted")
	}
	if strings.Contains(err.Error(), "not*base64") {
		t.Errorf("the error repeats the secret: %v", err)
	}
}

func TestDefaultSenderRefusesLoopback(t *testing.T) {
	var reached atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer srv.Close()

	// By name, so the address comes from DNS, as with a tenant's internal host.
	m, _ := testMessage(t, strings.Replace(srv.URL, "127.0.0.1", "localhost", 1))
	res := newSender(nil, time.Second).send(context.Background(), m)
	if res.ok() || !strings.Contains(res.errText, "not allowed (loopback address)") {
		t.Errorf("unexpected result: %+v", res)
	}
	if strings.Contains(res.errText, "127.0.0.1") || strings.Contains(res.errText, "::1") {
		t.Errorf("the error names the resolved address: %q", res.errText)
	}
	// The operator's log still gets the full story.
	if !strings.Contains(res.detail, "127.0.0.1") && !strings.Contains(res.detail, "::1") {
		t.Errorf("detail %q does not name the refused address", res.detail)
	}
	if reached.Load() {
		t.Error("the default sender reached a loopback server")
	}
}

// The delivery history is shown to the tenant, so it must not reveal what
// the worker's network looks like: resolved addresses, its own address, the
// DNS server. Go's error strings contain all of these.
func TestDescribeNamesNoAddresses(t *testing.T) {
	src := &net.TCPAddr{IP: net.ParseIP("172.18.0.5"), Port: 43210}
	dst := &net.TCPAddr{IP: net.ParseIP("10.1.2.3"), Port: 80}
	wrap := func(err error) error { return &url.Error{Op: "Post", URL: "http://db.internal.example/", Err: err} }
	tests := map[string]struct {
		err  error
		want string
	}{
		"blocked destination": {
			wrap(&net.OpError{Op: "dial", Net: "tcp", Addr: dst, Err: &safedial.BlockedError{Addr: netip.MustParseAddr("10.1.2.3"), Reason: "private network"}}),
			"not allowed (private network)",
		},
		"unknown host": {
			wrap(&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "db.internal.example", Server: "10.0.0.2:53", IsNotFound: true}}),
			"dns: no such host",
		},
		"resolver failure": {
			wrap(&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "server misbehaving", Name: "db.internal.example", Server: "10.0.0.2:53"}}),
			"dns: the host name could not be resolved",
		},
		"connection reset": {
			wrap(&net.OpError{Op: "read", Net: "tcp", Source: src, Addr: dst, Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}),
			"read: read: ",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := describe(tt.err, time.Second)
			if !strings.Contains(got, tt.want) {
				t.Errorf("describe = %q, want it to contain %q", got, tt.want)
			}
			for _, secret := range []string{"10.1.2.3", "172.18.0.5", "10.0.0.2", "43210", "db.internal.example"} {
				if strings.Contains(got, secret) {
					t.Errorf("describe = %q, which reveals %s", got, secret)
				}
			}
		})
	}
}

func TestSendNon2xxIsAFailure(t *testing.T) {
	// (1xx cannot be tested this way: Go's server sends it as an interim
	// response and then a 200.)
	for _, code := range []int{300, 304, 400, 404, 410, 500, 502} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		m, _ := testMessage(t, srv.URL)
		res := newSender(allowAll(), time.Second).send(context.Background(), m)
		srv.Close()
		if res.ok() || res.statusCode != code || !strings.Contains(res.errText, "HTTP") {
			t.Errorf("status %d: %+v, want a failure carrying the code", code, res)
		}
	}
	for _, code := range []int{200, 201, 202, 204, 299} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		m, _ := testMessage(t, srv.URL)
		res := newSender(allowAll(), time.Second).send(context.Background(), m)
		srv.Close()
		if !res.ok() {
			t.Errorf("status %d is a success: %+v", code, res)
		}
	}
}

func TestSendDoesNotFollowRedirects(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetHits.Add(1) }))
	defer target.Close()
	for _, code := range []int{301, 302, 303, 307, 308} {
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, code)
		}))
		m, _ := testMessage(t, redirector.URL)
		res := newSender(allowAll(), time.Second).send(context.Background(), m)
		redirector.Close()
		if res.ok() || res.statusCode != code {
			t.Errorf("%d: %+v, want a failure with the redirect status", code, res)
		}
	}
	if n := targetHits.Load(); n != 0 {
		t.Errorf("the redirect target was contacted %d times", n)
	}
}

func TestSendTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Reading the body first lets the server notice when the client
		// hangs up; otherwise closing the test server waits the full 5s.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	m, _ := testMessage(t, srv.URL)
	start := time.Now()
	res := newSender(allowAll(), 200*time.Millisecond).send(context.Background(), m)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("send took %s, the timeout was not enforced", elapsed)
	}
	if res.ok() || res.statusCode != 0 || !strings.Contains(res.errText, "timeout") {
		t.Errorf("unexpected result: %+v", res)
	}
	if res.duration < 150*time.Millisecond { // timers may fire slightly early
		t.Errorf("duration %s is shorter than the timeout", res.duration)
	}
}

func TestSendTimeoutCoversASlowBody(t *testing.T) {
	// Headers arrive at once, the body never ends: the timeout must still
	// end the attempt.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	m, _ := testMessage(t, srv.URL)
	start := time.Now()
	res := newSender(allowAll(), 300*time.Millisecond).send(context.Background(), m)
	if time.Since(start) > 2*time.Second {
		t.Fatal("a never-ending body kept the attempt open past the timeout")
	}
	// The 2xx already arrived, so this counts as delivered.
	if !res.ok() || res.statusCode != 200 {
		t.Errorf("unexpected result: %+v", res)
	}
}

func TestSendConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens there now

	m, _ := testMessage(t, "http://"+addr)
	res := newSender(allowAll(), time.Second).send(context.Background(), m)
	if res.ok() || res.statusCode != 0 || !strings.Contains(res.errText, "refused") {
		t.Errorf("unexpected result: %+v", res)
	}
	if strings.Contains(res.errText, "Post ") {
		t.Errorf("error repeats the request line: %q", res.errText)
	}
	if strings.Contains(res.errText, "127.0.0.1") {
		t.Errorf("error names an address: %q", res.errText)
	}
}

func TestSendReadsRetryAfterOnlyFor429And503(t *testing.T) {
	for code, want := range map[int]time.Duration{429: 7 * time.Second, 503: 7 * time.Second, 500: 0} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(code)
		}))
		m, _ := testMessage(t, srv.URL)
		res := newSender(allowAll(), time.Second).send(context.Background(), m)
		srv.Close()
		if res.retryAfter != want {
			t.Errorf("%d: retryAfter = %s, want %s", code, res.retryAfter, want)
		}
	}
}

func TestSendLimitsAndCleansTheSnippet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, "\x00\xff"+strings.Repeat("é", 100_000)) // ~200 KB
	}))
	defer srv.Close()
	m, _ := testMessage(t, srv.URL)
	res := newSender(allowAll(), time.Second).send(context.Background(), m)
	if res.statusCode != 500 || len([]rune(res.snippet)) > maxSnippetRunes || strings.ContainsRune(res.snippet, 0) {
		t.Errorf("snippet not limited/cleaned: status %d, %d runes", res.statusCode, len([]rune(res.snippet)))
	}
}

func TestSendIgnoresProxyEnvironment(t *testing.T) {
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { proxyHits.Add(1) }))
	defer proxy.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer target.Close()

	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	// Point the request at a non-loopback name so Go's own "no proxy for
	// localhost" rule cannot be what keeps the proxy out of the way.
	tr := allowAll()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, strings.TrimPrefix(target.URL, "http://"))
	}
	m, _ := testMessage(t, "http://hooks.example.invalid/")
	res := newSender(tr, time.Second).send(context.Background(), m)
	if !res.ok() {
		t.Fatalf("request failed: %+v", res)
	}
	if n := proxyHits.Load(); n != 0 {
		t.Errorf("the request went through HTTP_PROXY (%d hits)", n)
	}
}
