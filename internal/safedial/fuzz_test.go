package safedial

import (
	"net/netip"
	"strings"
	"testing"
)

// embeddedIPv4 lists the IPv4 addresses an IPv6 address carries in the schemes
// that make that possible, so the fuzz property can be stated without
// repeating the policy's own rules.
func embeddedIPv4(a netip.Addr) []netip.Addr {
	if !a.Is6() {
		return nil
	}
	b := a.As16()
	v4 := func(x, y, z, w byte) netip.Addr { return netip.AddrFrom4([4]byte{x, y, z, w}) }
	var out []netip.Addr
	// IPv4-mapped ::ffff:a.b.c.d
	if b[0]|b[1]|b[2]|b[3]|b[4]|b[5]|b[6]|b[7]|b[8]|b[9] == 0 && b[10] == 0xff && b[11] == 0xff {
		out = append(out, v4(b[12], b[13], b[14], b[15]))
	}
	// NAT64 64:ff9b::/96
	if b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b && b[4]|b[5]|b[6]|b[7]|b[8]|b[9]|b[10]|b[11] == 0 {
		out = append(out, v4(b[12], b[13], b[14], b[15]))
	}
	// 6to4 2002:AABB:CCDD::/48
	if b[0] == 0x20 && b[1] == 0x02 {
		out = append(out, v4(b[2], b[3], b[4], b[5]))
	}
	return out
}

func FuzzCheckConsistency(f *testing.F) {
	for _, s := range []string{"127.0.0.1", "10.0.0.1", "::ffff:100.64.0.1", "64:ff9b::a00:1", "2002:7f00:1::1", "2606:4700:4700::1111", "8.8.8.8", "::", "fe80::1"} {
		b := netip.MustParseAddr(s).As16()
		f.Add(b[:])
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) != 16 {
			return
		}
		var b [16]byte
		copy(b[:], raw)
		addr := netip.AddrFrom16(b)
		p := Policy{}

		// Whether an address is allowed must not depend on how it is written.
		if (p.Check(addr) == nil) != (p.Check(addr.Unmap()) == nil) {
			t.Fatalf("%s and its unmapped form %s are judged differently", addr, addr.Unmap())
		}
		// An IPv6 address that carries a blocked IPv4 address must be blocked.
		if p.Check(addr) == nil {
			for _, v4 := range embeddedIPv4(addr) {
				if p.Check(v4) != nil {
					t.Fatalf("%s is allowed but carries the blocked IPv4 address %s", addr, v4)
				}
			}
		}
		// AllowPrivate really is the only switch.
		if err := (Policy{AllowPrivate: true}).Check(addr); err != nil {
			t.Fatalf("AllowPrivate still refused %s: %v", addr, err)
		}
	})
}

func FuzzCheckIPv4(f *testing.F) {
	f.Add(uint32(0x7f000001))
	f.Add(uint32(0x0a000001))
	f.Add(uint32(0xa9fea9fe))
	f.Fuzz(func(t *testing.T, n uint32) {
		v4 := netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
		p := Policy{}
		// The same address in IPv4-mapped IPv6 form must be judged identically.
		if (p.Check(v4) == nil) != (p.Check(netip.AddrFrom16(v4.As16())) == nil) {
			t.Fatalf("%s is judged differently when mapped into IPv6", v4)
		}
		// Loopback, private, link-local and multicast are never allowed.
		if v4.IsLoopback() || v4.IsPrivate() || v4.IsLinkLocalUnicast() || v4.IsMulticast() || v4.IsUnspecified() {
			if p.Check(v4) == nil {
				t.Fatalf("%s is allowed", v4)
			}
		}
	})
}

func FuzzCheckHost(f *testing.F) {
	for _, s := range []string{"localhost", "LOCALHOST.", "a.localhost", "127.0.0.1", "::1", "fe80::1%eth0", "example.com", "", "%", "[::1]", "1.2.3.4.5", "::ffff:10.0.0.1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, host string) {
		p := Policy{}
		err := p.CheckHost(host) // must never panic
		h := strings.TrimSuffix(strings.ToLower(host), ".")
		if h == "localhost" || strings.HasSuffix(h, ".localhost") {
			if err == nil {
				t.Fatalf("%q is a localhost name but was allowed", host)
			}
		}
		// A host that is an IP literal gets exactly the verdict of Check.
		lit := h
		if i := strings.IndexByte(lit, '%'); i >= 0 {
			lit = lit[:i]
		}
		if addr, perr := netip.ParseAddr(lit); perr == nil && h != "localhost" && !strings.HasSuffix(h, ".localhost") {
			if (err == nil) != (p.Check(addr) == nil) {
				t.Fatalf("CheckHost(%q) = %v disagrees with Check(%s)", host, err, addr)
			}
		}
		if (Policy{AllowPrivate: true}).CheckHost(host) != nil {
			t.Fatalf("AllowPrivate still refused %q", host)
		}
	})
}
