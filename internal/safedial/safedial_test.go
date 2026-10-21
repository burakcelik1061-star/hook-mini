package safedial

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckBlocksNonPublicAddresses(t *testing.T) {
	blocked := map[string]string{
		"127.0.0.1":              "loopback",
		"127.255.255.254":        "loopback",
		"::1":                    "loopback",
		"10.0.0.1":               "private",
		"172.16.0.1":             "private",
		"172.31.255.255":         "private",
		"192.168.1.1":            "private",
		"fc00::1":                "private",
		"fd12:3456::1":           "private",
		"169.254.169.254":        "link-local", // AWS/GCP/Azure metadata
		"169.254.0.1":            "link-local",
		"fe80::1":                "link-local",
		"0.0.0.0":                "unspecified",
		"::":                     "unspecified",
		"0.1.2.3":                "this network",
		"100.64.0.1":             "carrier-grade NAT",
		"100.127.255.255":        "carrier-grade NAT",
		"192.0.0.8":              "IETF",
		"192.0.2.10":             "documentation",
		"198.18.0.1":             "benchmarking",
		"198.51.100.7":           "documentation",
		"203.0.113.9":            "documentation",
		"240.0.0.1":              "reserved",
		"255.255.255.255":        "broadcast",
		"224.0.0.1":              "multicast",
		"ff02::1":                "multicast",
		"::ffff:127.0.0.1":       "loopback",   // IPv4-mapped IPv6
		"::ffff:10.1.2.3":        "private",    // IPv4-mapped IPv6
		"::ffff:169.254.169.254": "link-local", // IPv4-mapped metadata
		// The netip predicates unmap on their own, but the prefix checks and
		// the broadcast comparison do not: these need the explicit Unmap.
		"::ffff:100.64.0.1":      "carrier-grade NAT",
		"::ffff:198.18.0.1":      "benchmarking",
		"::ffff:255.255.255.255": "broadcast",
		"64:ff9b::a00:1":         "NAT64", // NAT64 of 10.0.0.1
		"64:ff9b:1::1":           "local NAT64",
		"2002:7f00:1::1":         "6to4", // 6to4 of 127.0.0.1
		"2001:0:4136:e378::1":    "Teredo",
		"2001:db8::1":            "documentation",
		"100::1":                 "discard",
	}
	p := Policy{}
	for s, want := range blocked {
		err := p.Check(netip.MustParseAddr(s))
		var be *BlockedError
		if !errors.As(err, &be) {
			t.Errorf("%s: allowed, want blocked (%s)", s, want)
			continue
		}
		if !strings.Contains(be.Reason, want) {
			t.Errorf("%s: reason %q, want it to mention %q", s, be.Reason, want)
		}
	}
}

func TestCheckAllowsPublicAddresses(t *testing.T) {
	p := Policy{}
	for _, s := range []string{
		"1.1.1.1", "8.8.8.8", "93.184.216.34", "172.15.255.255", "172.32.0.1",
		"100.63.255.255", "100.128.0.1", "192.169.0.1", "2606:4700:4700::1111", "2a00:1450:4001::200e",
		"::ffff:8.8.8.8",
	} {
		if err := p.Check(netip.MustParseAddr(s)); err != nil {
			t.Errorf("%s: %v, want allowed", s, err)
		}
	}
}

func TestAllowPrivateLetsEverythingThrough(t *testing.T) {
	p := Policy{AllowPrivate: true}
	for _, s := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "::1"} {
		if err := p.Check(netip.MustParseAddr(s)); err != nil {
			t.Errorf("%s blocked although private addresses are allowed: %v", s, err)
		}
	}
}

func TestCheckHost(t *testing.T) {
	p := Policy{}
	for _, h := range []string{"localhost", "LOCALHOST", "localhost.", "api.localhost", "127.0.0.1", "::1", "10.0.0.5", "169.254.169.254", "fe80::1%eth0", "::ffff:127.0.0.1"} {
		if p.CheckHost(h) == nil {
			t.Errorf("CheckHost(%q) allowed", h)
		}
	}
	// Names are judged at dial time, not here: no DNS lookups at registration.
	for _, h := range []string{"example.com", "hooks.example.invalid", "8.8.8.8", "2606:4700:4700::1111", "localhost-but-not.example"} {
		if err := p.CheckHost(h); err != nil {
			t.Errorf("CheckHost(%q) = %v, want allowed", h, err)
		}
	}
	if err := (Policy{AllowPrivate: true}).CheckHost("localhost"); err != nil {
		t.Errorf("AllowPrivate must allow localhost: %v", err)
	}
	if err := p.CheckHost("localhost"); !strings.Contains(err.Error(), "destination is not allowed: localhost") {
		t.Errorf("unexpected message: %v", err)
	}
}

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "reached") }))
	t.Cleanup(srv.Close)
	return srv
}

func get(tr *http.Transport, url string) error {
	c := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func TestTransportRefusesLoopbackAtDialTime(t *testing.T) {
	srv := newServer(t) // listens on 127.0.0.1

	err := get(Policy{}.Transport(), srv.URL)
	var be *BlockedError
	if !errors.As(err, &be) || be.Reason != "loopback address" {
		t.Fatalf("err = %v, want a loopback BlockedError", err)
	}
	if err := get(Policy{AllowPrivate: true}.Transport(), srv.URL); err != nil {
		t.Fatalf("with AllowPrivate the request must succeed: %v", err)
	}
}

// The check must apply to the resolved address, not the name in the URL:
// "localhost" is a name that resolves to loopback, standing in for a public
// hostname whose DNS was changed to point inside (DNS rebinding).
func TestTransportChecksTheResolvedAddressNotTheName(t *testing.T) {
	srv := newServer(t)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	err := get(Policy{}.Transport(), "http://localhost:"+port+"/")
	var be *BlockedError
	if !errors.As(err, &be) || !be.Addr.IsLoopback() {
		t.Fatalf("err = %v, want the resolved loopback address to be blocked", err)
	}
}

func TestTransportIgnoresProxyEnvironment(t *testing.T) {
	// A proxy would make the connection on our behalf, out of the dialer's
	// sight, so it must never be used, even if the environment sets one.
	var reachedProxy atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reachedProxy.Store(true) }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)

	tr := Policy{}.Transport()
	if tr.Proxy != nil {
		t.Fatal("transport has a proxy function")
	}
	// A public address that will not answer; all that matters is that the
	// request is not handed to the proxy, so a short timeout is enough.
	c := &http.Client{Transport: tr, Timeout: 300 * time.Millisecond}
	if resp, err := c.Get("http://192.88.99.1:9/"); err == nil {
		resp.Body.Close()
	}
	if reachedProxy.Load() {
		t.Error("the request went to HTTP_PROXY")
	}
}

func TestDialerAppliesToEveryConnection(t *testing.T) {
	// A raw dial (not HTTP) through the policy dialer is refused too.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, err = Policy{}.Dialer().DialContext(context.Background(), "tcp", ln.Addr().String())
	var be *BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("raw dial to loopback: err = %v, want BlockedError", err)
	}
}
