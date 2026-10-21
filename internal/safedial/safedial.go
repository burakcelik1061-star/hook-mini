// Package safedial keeps webhook traffic away from addresses it must never
// reach: the worker's own host, private networks and cloud metadata services.
//
// The check runs when a connection is opened, against the IP address actually
// being dialled, after DNS resolution. Validating the URL when an endpoint is
// registered is not enough on its own: a hostname that resolves to a public
// address today can resolve to 127.0.0.1 tomorrow (DNS rebinding), and a
// hostname can return several addresses. net.Dialer.Control sees every one.
package safedial

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"
)

// Policy decides which destination addresses may be contacted.
type Policy struct {
	// AllowPrivate permits loopback, private and other non-public
	// addresses. It exists for local development and tests (a receiver on
	// the Docker network has a private address) and must stay off in
	// production.
	AllowPrivate bool
}

// BlockedError reports a destination the policy refused.
type BlockedError struct {
	Addr   netip.Addr
	Reason string
}

func (e *BlockedError) Error() string {
	if !e.Addr.IsValid() {
		return "destination is not allowed: " + e.Reason
	}
	return fmt.Sprintf("destination %s is not allowed: %s", e.Addr, e.Reason)
}

// blockedPrefixes are ranges that are not ordinary public unicast space, in
// addition to what the netip predicates already cover. Several of them embed
// an IPv4 address in IPv6 and could otherwise smuggle a private one through.
var blockedPrefixes = []struct {
	prefix netip.Prefix
	reason string
}{
	{netip.MustParsePrefix("0.0.0.0/8"), `"this network"`},
	{netip.MustParsePrefix("100.64.0.0/10"), "carrier-grade NAT"},
	{netip.MustParsePrefix("192.0.0.0/24"), "IETF protocol assignments"},
	{netip.MustParsePrefix("192.0.2.0/24"), "documentation"},
	{netip.MustParsePrefix("198.18.0.0/15"), "benchmarking"},
	{netip.MustParsePrefix("198.51.100.0/24"), "documentation"},
	{netip.MustParsePrefix("203.0.113.0/24"), "documentation"},
	{netip.MustParsePrefix("240.0.0.0/4"), "reserved"},
	// A public address, but on Azure it is the platform's WireServer, which
	// answers only from inside a VM (host agent configuration, health probes).
	// The metadata services of other clouds sit in ranges blocked above
	// (169.254.169.254, 100.100.100.200, fd00:ec2::254).
	{netip.MustParsePrefix("168.63.129.16/32"), "cloud platform service (Azure WireServer)"},
	{netip.MustParsePrefix("64:ff9b::/96"), "NAT64 (embeds an IPv4 address)"},
	{netip.MustParsePrefix("64:ff9b:1::/48"), "local NAT64 (embeds an IPv4 address)"},
	{netip.MustParsePrefix("100::/64"), "discard-only"},
	{netip.MustParsePrefix("2001::/32"), "Teredo (embeds an IPv4 address)"},
	{netip.MustParsePrefix("2001::/23"), "IETF protocol assignments"},
	{netip.MustParsePrefix("2001:db8::/32"), "documentation"},
	{netip.MustParsePrefix("2002::/16"), "6to4 (embeds an IPv4 address)"},
	{netip.MustParsePrefix("3fff::/20"), "documentation"},
}

// globalUnicast6 is the only IPv6 range assigned for global unicast. IPv6 is
// judged by this allowlist rather than by a list of exceptions: the
// remaining space holds deprecated and special forms (IPv4-compatible
// ::a.b.c.d, SIIT ::ffff:0:a.b.c.d, site-local fec0::/10) that some networks
// still route inside, and that netip.Addr.IsGlobalUnicast accepts.
var globalUnicast6 = netip.MustParsePrefix("2000::/3")

// Check reports whether addr may be contacted.
func (p Policy) Check(addr netip.Addr) error {
	if p.AllowPrivate {
		return nil
	}
	// ::ffff:127.0.0.1 is 127.0.0.1; judge the IPv4 address it carries.
	addr = addr.Unmap()

	switch {
	case !addr.IsValid():
		return &BlockedError{Addr: addr, Reason: "invalid address"}
	case addr.IsLoopback():
		return &BlockedError{Addr: addr, Reason: "loopback address"}
	case addr.IsPrivate():
		return &BlockedError{Addr: addr, Reason: "private network"}
	case addr.IsLinkLocalUnicast():
		// Includes 169.254.169.254, the metadata service of most clouds.
		return &BlockedError{Addr: addr, Reason: "link-local address (includes cloud metadata services)"}
	case addr.IsUnspecified():
		return &BlockedError{Addr: addr, Reason: "unspecified address"}
	case addr.IsMulticast(), addr.IsLinkLocalMulticast(), addr.IsInterfaceLocalMulticast():
		return &BlockedError{Addr: addr, Reason: "multicast address"}
	case addr == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return &BlockedError{Addr: addr, Reason: "broadcast address"}
	}
	for _, b := range blockedPrefixes {
		if b.prefix.Contains(addr) {
			return &BlockedError{Addr: addr, Reason: b.reason}
		}
	}
	if addr.Is6() && !globalUnicast6.Contains(addr) {
		return &BlockedError{Addr: addr, Reason: "outside the IPv6 global unicast range (2000::/3)"}
	}
	if !addr.IsGlobalUnicast() {
		return &BlockedError{Addr: addr, Reason: "not a public unicast address"}
	}
	return nil
}

// control is a net.Dialer Control hook. It runs for every connection attempt,
// after name resolution, with the exact address about to be connected to.
func (p Policy) control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("unexpected dial address %q: %w", address, err)
	}
	return p.Check(ap.Addr())
}

// maxResponseHeaderBytes bounds the response headers a Transport accepts.
const maxResponseHeaderBytes = 64 << 10

// Dialer returns a dialer that refuses addresses the policy blocks.
func (p Policy) Dialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   p.control,
	}
}

// Transport returns an HTTP transport for webhook traffic: every connection
// goes through the policy's dialer, and proxy settings from the environment
// are ignored (a proxy would dial on our behalf, out of the policy's sight).
func (p Policy) Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DialContext = p.Dialer().DialContext
	t.MaxIdleConnsPerHost = 16
	// Only the status line and Retry-After are of any use; the 1 MB default
	// would let each endpoint make every attempt allocate a megabyte.
	t.MaxResponseHeaderBytes = maxResponseHeaderBytes
	return t
}

// CheckHost is the early check used when an endpoint URL is registered. It
// rejects what is knowable without DNS: IP literals the policy blocks and the
// reserved "localhost" names. Everything else is judged again at dial time.
func (p Policy) CheckHost(host string) error {
	if p.AllowPrivate {
		return nil
	}
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return &BlockedError{Reason: "localhost names always point at the local host"}
	}
	// Zone identifiers (fe80::1%eth0) only occur on link-local addresses.
	if i := strings.IndexByte(h, '%'); i >= 0 {
		h = h[:i]
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		return p.Check(addr)
	}
	return nil
}
