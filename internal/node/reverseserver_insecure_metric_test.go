package node

import (
	"crypto/tls"
	"net/http"
	"testing"
)

// TestReverseUpgrader_InsecureMetric verifies the #1026 observability
// counter: naozhi_node_insecure_reverse_upgrade_total bumps whenever a
// reverse-node WS upgrade arrives over plain HTTP from a non-loopback host
// (cleartext bearer token on the wire) and stays put for the TLS, loopback,
// and Origin-bearing (rejected) cases.
//
// R20260606-SEC-4 (#1824): a plain-HTTP public/routable host is now
// HARD-REJECTED (wantOK=false) — the token would otherwise traverse the open
// internet in cleartext. The counter still bumps so /debug/vars records the
// rejected exposure attempt. Private-LAN plain HTTP is still accepted.
func TestReverseUpgrader_InsecureMetric(t *testing.T) {
	check := reverseUpgrader.CheckOrigin

	cases := []struct {
		name      string
		req       *http.Request
		wantOK    bool
		wantDelta int64
	}{
		{
			name:      "plain http public/routable host rejected but counts",
			req:       &http.Request{Host: "worker.internal:8080", Header: http.Header{}},
			wantOK:    false,
			wantDelta: 1,
		},
		{
			name:      "plain http private-LAN host accepted and counts",
			req:       &http.Request{Host: "192.168.10.10:8080", Header: http.Header{}},
			wantOK:    true,
			wantDelta: 1,
		},
		{
			name:      "tls direct termination does not count",
			req:       &http.Request{Host: "worker.internal:8080", Header: http.Header{}, TLS: &tls.ConnectionState{}},
			wantOK:    true,
			wantDelta: 0,
		},
		{
			name:      "loopback does not count",
			req:       &http.Request{Host: "127.0.0.1:8080", Header: http.Header{}},
			wantOK:    true,
			wantDelta: 0,
		},
		{
			name:      "origin header rejected and does not count",
			req:       &http.Request{Host: "worker.internal:8080", Header: http.Header{"Origin": {"https://evil.example"}}},
			wantOK:    false,
			wantDelta: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := insecureReverseUpgradeTotal.Value()
			got := check(tc.req)
			delta := insecureReverseUpgradeTotal.Value() - before
			if got != tc.wantOK {
				t.Errorf("CheckOrigin = %v, want %v", got, tc.wantOK)
			}
			if delta != tc.wantDelta {
				t.Errorf("insecureReverseUpgradeTotal delta = %d, want %d", delta, tc.wantDelta)
			}
		})
	}
}

// TestReverseUpgrader_InsecureMetric_Monotonic confirms the counter bumps
// on EVERY insecure upgrade, not just the first — the once-guarded log
// hides repeat exposure, which is the gap the metric closes (#1026). Uses a
// private-LAN host so the upgrade is still accepted post-#1824 while the
// counter keeps climbing.
func TestReverseUpgrader_InsecureMetric_Monotonic(t *testing.T) {
	check := reverseUpgrader.CheckOrigin
	req := &http.Request{Host: "10.1.2.3:8080", Header: http.Header{}}

	before := insecureReverseUpgradeTotal.Value()
	const n = 3
	for i := 0; i < n; i++ {
		if !check(req) {
			t.Fatalf("CheckOrigin rejected a plain-http private-LAN request")
		}
	}
	if delta := insecureReverseUpgradeTotal.Value() - before; delta != n {
		t.Errorf("counter delta after %d insecure upgrades = %d, want %d", n, delta, n)
	}
}

// TestReverseUpgrader_PublicPlainHTTPRejected covers R20260606-SEC-4 (#1824):
// a plain-HTTP reverse upgrade from a public/routable Host (no TLS, no Origin)
// is hard-rejected so the bearer token never rides the first frame in
// cleartext over the open internet. The cleartext-exposure counter still bumps
// to record the rejected attempt on /debug/vars. TLS-terminated and
// loopback/private wiring stay unaffected (covered above).
func TestReverseUpgrader_PublicPlainHTTPRejected(t *testing.T) {
	check := reverseUpgrader.CheckOrigin
	cases := []string{
		"worker.internal:8080", // public hostname (resolution unknown → routable)
		"8.8.8.8:8080",         // public IPv4 literal
		"[2606:4700::1111]:80", // public IPv6 literal
		"hub.example.com",      // public hostname, no port
	}
	for _, host := range cases {
		req := &http.Request{Host: host, Header: http.Header{}}
		before := insecureReverseUpgradeTotal.Value()
		if check(req) {
			t.Errorf("CheckOrigin(%q) accepted a plain-HTTP public-host upgrade; want rejected", host)
		}
		if delta := insecureReverseUpgradeTotal.Value() - before; delta != 1 {
			t.Errorf("CheckOrigin(%q) counter delta = %d, want 1 (rejected attempt still recorded)", host, delta)
		}
	}
}

// TestReverseUpgrader_PublicPlainHTTP_TLSStillAccepted confirms the #1824
// reject only targets plain HTTP: the same public Host over TLS (proxy/direct
// termination) is still accepted, since the token is then encrypted.
func TestReverseUpgrader_PublicPlainHTTP_TLSStillAccepted(t *testing.T) {
	check := reverseUpgrader.CheckOrigin
	req := &http.Request{Host: "hub.example.com", Header: http.Header{}, TLS: &tls.ConnectionState{}}
	before := insecureReverseUpgradeTotal.Value()
	if !check(req) {
		t.Fatalf("CheckOrigin rejected a TLS-terminated public-host upgrade; want accepted")
	}
	if delta := insecureReverseUpgradeTotal.Value() - before; delta != 0 {
		t.Errorf("TLS upgrade bumped cleartext counter by %d, want 0", delta)
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

// TestReverseUpgrader_PrivateHostStillAccepted confirms a plain-HTTP RFC1918
// upgrade is still accepted (no behavior break for no-TLS private deployments)
// and still bumps the cleartext-exposure counter (#1593).
func TestReverseUpgrader_PrivateHostStillAccepted(t *testing.T) {
	check := reverseUpgrader.CheckOrigin
	req := &http.Request{Host: "192.168.50.50:8080", Header: http.Header{}}
	before := insecureReverseUpgradeTotal.Value()
	if !check(req) {
		t.Fatalf("CheckOrigin rejected a plain-http private-LAN request; want accepted")
	}
	if delta := insecureReverseUpgradeTotal.Value() - before; delta != 1 {
		t.Errorf("counter delta for private-LAN upgrade = %d, want 1", delta)
	}
}
