package envpolicy

import (
	"net"
	"testing"
)

// TestClassifyIP_Matrix pins the classification of every SSRF-relevant range
// the three call sites (baseurl.go, internal/shim, internal/node) depend on,
// including the edge literals a hand-rolled classifier tends to get wrong:
// IPv4-mapped IPv6, the unspecified address, link-local multicast, and the
// RFC1918 172.16/12 boundary. #2300.
func TestClassifyIP_Matrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ip   string
		want IPClass
	}{
		// loopback
		{"127.0.0.1", IPLoopback},
		{"127.255.255.254", IPLoopback},
		{"::1", IPLoopback},
		{"::ffff:127.0.0.1", IPLoopback},

		// link-local unicast (IMDS lives here)
		{"169.254.169.254", IPLinkLocalUnicast},
		{"169.254.1.1", IPLinkLocalUnicast},
		{"fe80::1", IPLinkLocalUnicast},
		{"::ffff:169.254.169.254", IPLinkLocalUnicast},

		// link-local multicast
		{"224.0.0.1", IPLinkLocalMulticast},
		{"224.0.0.251", IPLinkLocalMulticast},
		{"ff02::1", IPLinkLocalMulticast},
		{"::ffff:224.0.0.1", IPLinkLocalMulticast},

		// private RFC1918 / ULA
		{"10.0.0.1", IPPrivate},
		{"10.255.255.255", IPPrivate},
		{"172.16.0.1", IPPrivate},
		{"172.31.255.255", IPPrivate},
		{"192.168.1.1", IPPrivate},
		{"fc00::1", IPPrivate},
		{"fd00::1", IPPrivate},
		{"::ffff:10.0.0.1", IPPrivate},
		{"::ffff:192.168.0.1", IPPrivate},

		// unspecified
		{"0.0.0.0", IPUnspecified},
		{"::", IPUnspecified},
		{"::ffff:0.0.0.0", IPUnspecified},

		// public / not in any class
		{"8.8.8.8", 0},
		{"1.1.1.1", 0},
		{"172.32.0.1", 0}, // just past RFC1918 172.16/12
		{"172.15.255.255", 0},
		{"192.167.255.255", 0},
		{"11.0.0.0", 0},
		{"100.64.0.1", 0}, // CGNAT shared range: NOT private per net.IP
		{"2001:db8::1", 0},
		{"224.0.1.1", 0}, // multicast but not link-local scope
		{"::ffff:8.8.8.8", 0},
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("test table bug: %q does not parse", tc.ip)
			}
			if got := ClassifyIP(ip); got != tc.want {
				t.Errorf("ClassifyIP(%q) = %08b, want %08b", tc.ip, got, tc.want)
			}
		})
	}
}

// TestClassifyIP_ClassesAreDisjoint pins that no literal lands in two classes
// at once. Callers such as the shim guard rely on this to subtract loopback
// from the internal deny-set without accidentally un-denying another range.
func TestClassifyIP_ClassesAreDisjoint(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		"127.0.0.1", "::1", "169.254.169.254", "fe80::1", "224.0.0.1", "ff02::1",
		"10.0.0.1", "172.16.0.1", "192.168.1.1", "fd00::1", "0.0.0.0", "::",
		"::ffff:10.0.0.1", "::ffff:127.0.0.1", "::ffff:169.254.169.254", "::ffff:0.0.0.0",
	} {
		k := ClassifyIP(net.ParseIP(s))
		if k == 0 {
			t.Errorf("%q: expected some class, got 0", s)
			continue
		}
		if k&(k-1) != 0 {
			t.Errorf("%q: classified into multiple classes %08b; classes must be disjoint", s, k)
		}
	}
}

func TestClassifyIP_NilAndMalformed(t *testing.T) {
	t.Parallel()
	if got := ClassifyIP(nil); got != 0 {
		t.Errorf("ClassifyIP(nil) = %08b, want 0", got)
	}
	// A net.IP of bogus length must not panic and must classify as nothing;
	// callers treat 0 as "not a literal IP", never as "public and safe".
	for _, bogus := range []net.IP{{}, {1}, {1, 2, 3}, {1, 2, 3, 4, 5}} {
		if got := ClassifyIP(bogus); got != 0 {
			t.Errorf("ClassifyIP(%v) = %08b, want 0", []byte(bogus), got)
		}
	}
}

func TestClassifyHost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		host   string
		want   IPClass
		wantOK bool
	}{
		{"10.0.0.1", IPPrivate, true},
		{"::1", IPLoopback, true},
		{"::ffff:10.0.0.1", IPPrivate, true},
		{"169.254.169.254", IPLinkLocalUnicast, true},
		{"0.0.0.0", IPUnspecified, true},
		{"8.8.8.8", 0, true},
		// Not literals: hostnames, bracketed/ported forms the caller must
		// strip first, garbage. All must come back ok=false, class 0.
		{"localhost", 0, false},
		{"example.com", 0, false},
		{"[::1]", 0, false},
		{"10.0.0.1:8080", 0, false},
		{"", 0, false},
		{"10.0.0", 0, false},
		{"999.1.1.1", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			got, ok := ClassifyHost(tc.host)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("ClassifyHost(%q) = (%08b, %v), want (%08b, %v)", tc.host, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestIPClass_HasAny(t *testing.T) {
	t.Parallel()
	k := IPPrivate
	if !k.Has(IPPrivate) || k.Has(IPLoopback) || k.Has(IPPrivate|IPLoopback) {
		t.Errorf("Has misbehaves for %08b", k)
	}
	if !k.Any(IPPrivate|IPLoopback) || k.Any(IPLoopback|IPLinkLocal) || k.Any(0) {
		t.Errorf("Any misbehaves for %08b", k)
	}
	if IPClass(0).Has(0) != true {
		t.Errorf("zero.Has(0) should be true (vacuous)")
	}
	// The composite constants must be exactly the union they document.
	if IPLinkLocal != IPLinkLocalUnicast|IPLinkLocalMulticast {
		t.Errorf("IPLinkLocal composite drifted: %08b", IPLinkLocal)
	}
	if IPInternalAny != IPLoopback|IPLinkLocalUnicast|IPLinkLocalMulticast|IPPrivate|IPUnspecified {
		t.Errorf("IPInternalAny composite drifted: %08b", IPInternalAny)
	}
}

// TestClassifyIP_MatchesStdlibPredicates is the equivalence pin for the three
// migrated call sites: for a broad literal sample, each class bit must equal
// the exact net.IP predicate the pre-#2300 hand-written code called. If this
// ever fails, some caller's allow/deny boundary has silently moved.
func TestClassifyIP_MatchesStdlibPredicates(t *testing.T) {
	t.Parallel()
	sample := []string{
		"0.0.0.0", "0.0.0.1", "8.8.8.8", "10.0.0.1", "10.255.255.255", "11.0.0.0",
		"100.64.0.1", "127.0.0.1", "127.0.0.2", "169.254.0.1", "169.254.169.254",
		"169.255.0.1", "172.15.255.255", "172.16.0.1", "172.31.255.255", "172.32.0.1",
		"192.167.255.255", "192.168.0.1", "192.168.255.255", "192.169.0.1",
		"224.0.0.1", "224.0.0.255", "224.0.1.1", "239.255.255.255", "255.255.255.255",
		"::", "::1", "::2", "2001:db8::1", "fc00::1", "fd00::1", "fdff::1", "fe00::1",
		"fe80::1", "febf::1", "fec0::1", "ff02::1", "ff05::1",
		"::ffff:0.0.0.0", "::ffff:8.8.8.8", "::ffff:10.0.0.1", "::ffff:127.0.0.1",
		"::ffff:169.254.169.254", "::ffff:172.16.0.1", "::ffff:192.168.1.1", "::ffff:224.0.0.1",
	}
	for _, s := range sample {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("test table bug: %q does not parse", s)
		}
		k := ClassifyIP(ip)
		check := func(name string, bit IPClass, want bool) {
			if got := k.Has(bit); got != want {
				t.Errorf("%q: %s = %v, stdlib says %v", s, name, got, want)
			}
		}
		check("IPLoopback", IPLoopback, ip.IsLoopback())
		check("IPLinkLocalUnicast", IPLinkLocalUnicast, ip.IsLinkLocalUnicast())
		check("IPLinkLocalMulticast", IPLinkLocalMulticast, ip.IsLinkLocalMulticast())
		check("IPPrivate", IPPrivate, ip.IsPrivate())
		check("IPUnspecified", IPUnspecified, ip.IsUnspecified())
	}
}
