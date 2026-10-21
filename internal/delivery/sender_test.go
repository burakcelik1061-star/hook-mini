package delivery

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testMessage(t *testing.T, url string) (message, uuid.UUID) {
	t.Helper()
	id := uuid.New()
	m, err := newMessage(url, id, "invoice.paid", time.Date(2026, 10, 21, 12, 0, 0, 123000000, time.FixedZone("TRT", 3*3600)), json.RawMessage(`{"amount":42}`))
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
	res := newSender(nil, time.Second).send(context.Background(), m)

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

func TestSendNon2xxIsAFailure(t *testing.T) {
	// (1xx cannot be tested this way: Go's server sends it as an interim
	// response and then a 200.)
	for _, code := range []int{300, 304, 400, 404, 410, 500, 502} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		m, _ := testMessage(t, srv.URL)
		res := newSender(nil, time.Second).send(context.Background(), m)
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
		res := newSender(nil, time.Second).send(context.Background(), m)
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
		res := newSender(nil, time.Second).send(context.Background(), m)
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
	res := newSender(nil, 200*time.Millisecond).send(context.Background(), m)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("send took %s, the timeout was not enforced", elapsed)
	}
	if res.ok() || res.statusCode != 0 || !strings.Contains(res.errText, "timeout") {
		t.Errorf("unexpected result: %+v", res)
	}
	if res.duration < 200*time.Millisecond {
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
	res := newSender(nil, 300*time.Millisecond).send(context.Background(), m)
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
	res := newSender(nil, time.Second).send(context.Background(), m)
	if res.ok() || res.statusCode != 0 || !strings.Contains(res.errText, "refused") {
		t.Errorf("unexpected result: %+v", res)
	}
	if strings.Contains(res.errText, "Post ") {
		t.Errorf("error repeats the request line: %q", res.errText)
	}
}

func TestSendReadsRetryAfterOnlyFor429And503(t *testing.T) {
	for code, want := range map[int]time.Duration{429: 7 * time.Second, 503: 7 * time.Second, 500: 0} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(code)
		}))
		m, _ := testMessage(t, srv.URL)
		res := newSender(nil, time.Second).send(context.Background(), m)
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
	res := newSender(nil, time.Second).send(context.Background(), m)
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
	tr := defaultTransport()
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
