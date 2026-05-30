package node

import (
	"crypto/tls"
	"net/http"
	"testing"
)

// TestReverseUpgrader_InsecureMetric verifies the #1026 observability
// counter: naozhi_node_insecure_reverse_upgrade_total bumps exactly when
// a reverse-node WS upgrade is accepted over plain HTTP from a non-loopback
// host (cleartext bearer token on the wire) and stays put for the TLS,
// loopback, and Origin-bearing (rejected) cases.
func TestReverseUpgrader_InsecureMetric(t *testing.T) {
	check := reverseUpgrader.CheckOrigin

	cases := []struct {
		name      string
		req       *http.Request
		wantOK    bool
		wantDelta int64
	}{
		{
			name:      "plain http non-loopback counts",
			req:       &http.Request{Host: "worker.internal:8080", Header: http.Header{}},
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
// hides repeat exposure, which is the gap the metric closes (#1026).
func TestReverseUpgrader_InsecureMetric_Monotonic(t *testing.T) {
	check := reverseUpgrader.CheckOrigin
	req := &http.Request{Host: "worker.internal:8080", Header: http.Header{}}

	before := insecureReverseUpgradeTotal.Value()
	const n = 3
	for i := 0; i < n; i++ {
		if !check(req) {
			t.Fatalf("CheckOrigin rejected a plain-http non-loopback request")
		}
	}
	if delta := insecureReverseUpgradeTotal.Value() - before; delta != n {
		t.Errorf("counter delta after %d insecure upgrades = %d, want %d", n, delta, n)
	}
}
