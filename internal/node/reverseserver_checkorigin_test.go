package node

import (
	"crypto/tls"
	"net/http"
	"testing"
)

// TestReverseUpgrader_CheckOrigin: a plain-HTTP reverse upgrade from a
// public/routable Host is HARD-REJECTED (R20260606-SEC-4, #1824) so the bearer
// token never rides the first frame in cleartext over the open internet;
// private-LAN plain HTTP, TLS and loopback are accepted; an Origin header is
// always rejected.
func TestReverseUpgrader_CheckOrigin(t *testing.T) {
	check := reverseUpgrader.CheckOrigin
	cases := []struct {
		name   string
		req    *http.Request
		wantOK bool
	}{
		{"plain http public hostname", &http.Request{Host: "worker.internal:8080", Header: http.Header{}}, false},
		{"plain http public IPv4", &http.Request{Host: "8.8.8.8:8080", Header: http.Header{}}, false},
		{"plain http public IPv6", &http.Request{Host: "[2606:4700::1111]:80", Header: http.Header{}}, false},
		{"plain http public hostname no port", &http.Request{Host: "hub.example.com", Header: http.Header{}}, false},
		{"plain http private-LAN host", &http.Request{Host: "192.168.10.10:8080", Header: http.Header{}}, true},
		{"tls public host", &http.Request{Host: "hub.example.com", Header: http.Header{}, TLS: &tls.ConnectionState{}}, true},
		{"loopback", &http.Request{Host: "127.0.0.1:8080", Header: http.Header{}}, true},
		{"origin header", &http.Request{Host: "worker.internal:8080", Header: http.Header{"Origin": {"https://evil.example"}}}, false},
	}
	for _, tc := range cases {
		if got := check(tc.req); got != tc.wantOK {
			t.Errorf("%s: CheckOrigin = %v, want %v", tc.name, got, tc.wantOK)
		}
	}
}

// TestIsPrivateHost covers R164029-SEC-1 (#1593): RFC1918, IPv6 unique-local,
// and link-local literals are classified private; loopback, public, and
// non-IP hostnames are not. Port suffixes and bracketed IPv6 must be stripped.
func TestIsPrivateHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"192.168.1.10:8080", true},
		{"10.0.0.5", true},
		{"172.16.4.4:443", true},
		{"172.31.255.255", true},
		{"172.32.0.1", false}, // just outside RFC1918
		{"169.254.1.1", true}, // link-local
		{"[fd00::1]:8080", true},
		{"[fe80::1]", true},
		{"127.0.0.1:8080", false}, // loopback handled by isLoopbackHost, not private
		{"8.8.8.8:53", false},
		{"worker.internal:8080", false}, // hostname, not an IP literal
		{"example.com", false},
	}
	for _, tc := range cases {
		if got := isPrivateHost(tc.host); got != tc.want {
			t.Errorf("isPrivateHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}
