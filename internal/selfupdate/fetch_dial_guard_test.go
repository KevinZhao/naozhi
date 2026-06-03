package selfupdate

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// TestIsUnsafeResolvedIP pins R20260603030037-SEC-2 (#1616): the dial-time IP
// screen must reject the internal-address surface a DNS-poisoning attacker
// could redirect a github asset host to (IMDS, RFC1918, link-local, loopback,
// unspecified) while letting ordinary public addresses through.
func TestIsUnsafeResolvedIP(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ip       string
		unsafe   bool
		whatItIs string
	}{
		{"169.254.169.254", true, "IMDS link-local"},
		{"169.254.1.1", true, "IPv4 link-local"},
		{"fe80::1", true, "IPv6 link-local unicast"},
		{"ff02::1", true, "IPv6 link-local multicast"},
		{"10.0.0.5", true, "RFC1918 10/8"},
		{"172.16.4.4", true, "RFC1918 172.16/12"},
		{"192.168.1.1", true, "RFC1918 192.168/16"},
		{"fd00::1", true, "IPv6 ULA (private)"},
		{"127.0.0.1", true, "IPv4 loopback"},
		{"::1", true, "IPv6 loopback"},
		{"0.0.0.0", true, "unspecified"},
		{"::", true, "IPv6 unspecified"},
		{"8.8.8.8", false, "public IPv4"},
		{"140.82.121.3", false, "public github IPv4"},
		{"2606:50c0:8000::153", false, "public IPv6"},
	}
	for _, c := range cases {
		addr, err := netip.ParseAddr(c.ip)
		if err != nil {
			t.Fatalf("ParseAddr(%s): %v", c.ip, err)
		}
		if got := isUnsafeResolvedIP(addr); got != c.unsafe {
			t.Errorf("isUnsafeResolvedIP(%s) = %v, want %v (%s)", c.ip, got, c.unsafe, c.whatItIs)
		}
	}
	// An invalid/zero address must fail closed.
	if !isUnsafeResolvedIP(netip.Addr{}) {
		t.Errorf("isUnsafeResolvedIP(invalid) = false, want true (fail closed)")
	}
}

// TestGuardedDialContext_RejectsLoopback verifies the dialer actually refuses a
// connection whose resolved remote address is internal. Loopback is in the
// deny set, so dialing a real loopback listener must be rejected (and the
// connection closed) rather than handed back to the HTTP client.
func TestGuardedDialContext_RejectsLoopback(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := guardedDialContext(ctx, "tcp", ln.Addr().String())
	if err == nil {
		conn.Close()
		t.Fatalf("guardedDialContext accepted loopback dial to %s, want rejection", ln.Addr())
	}
	if !strings.Contains(err.Error(), "internal/link-local") {
		t.Errorf("unexpected error %q, want internal/link-local rejection", err.Error())
	}
}

// TestSafeFetchTransport_TestHatchPreserved asserts the sanctioned test escape
// hatch: when testHTTPTransport is set, safeFetchTransport returns it verbatim
// (so httptest loopback targets, which the dial guard would otherwise reject,
// keep working) and never installs the production dial guard.
func TestSafeFetchTransport_TestHatchPreserved(t *testing.T) {
	// Not parallel: mutates the package-level testHTTPTransport sentinel.
	prev := testHTTPTransport
	defer func() { testHTTPTransport = prev }()

	marker := &markerTransport{}
	testHTTPTransport = marker
	if got := safeFetchTransport(); got != marker {
		t.Fatalf("safeFetchTransport() = %T, want the injected testHTTPTransport", got)
	}

	// Production path: nil hatch → a guarded *http.Transport, not the
	// injected marker.
	testHTTPTransport = nil
	got := safeFetchTransport()
	if got == nil {
		t.Fatalf("safeFetchTransport() returned nil in production path")
	}
	if _, isMarker := got.(*markerTransport); isMarker {
		t.Fatalf("production transport leaked the test marker type")
	}
	if _, isTransport := got.(*http.Transport); !isTransport {
		t.Fatalf("production transport = %T, want *http.Transport with dial guard", got)
	}
}

type markerTransport struct{}

func (m *markerTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }
