package server

import (
	"strings"
	"testing"
)

// TestShouldWarnReverseNodePlaintext_Matrix locks the decision matrix for the
// R176-SEC-MED /ws-node plaintext warning. Four axes produce the truth table:
// reverse-server configured / not × loopback / public bind × trustedProxy
// true / false. A silent regression that suppresses the warning (e.g. inverted
// condition, accidentally treating a public IP as loopback) would let an
// operator ship a plaintext-exposed reverse-node endpoint without the startup
// journal line firing.
func TestShouldWarnReverseNodePlaintext_Matrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                 string
		reverseServerEnabled bool
		trustedProxy         bool
		addr                 string
		want                 bool
	}{
		// Feature inactive: never warn regardless of addr/proxy.
		{"no_reverse_server_loopback", false, false, "127.0.0.1:8180", false},
		{"no_reverse_server_public", false, false, "0.0.0.0:8180", false},
		{"no_reverse_server_public_proxied", false, true, "0.0.0.0:8180", false},

		// Reverse server on loopback: never warn (traffic stays on host).
		{"reverse_loopback_v4", true, false, "127.0.0.1:8180", false},
		{"reverse_loopback_v6", true, false, "[::1]:8180", false},
		{"reverse_loopback_name", true, false, "localhost:8180", false},

		// Reverse server + trusted proxy: no warn (upstream terminates TLS).
		{"reverse_public_proxied_v4", true, true, "0.0.0.0:8180", false},
		{"reverse_public_proxied_v6", true, true, "[::]:8180", false},
		{"reverse_public_proxied_bareport", true, true, ":8180", false},
		{"reverse_public_proxied_lan", true, true, "192.168.1.5:8180", false},

		// Reverse server on public bind with no trusted proxy: WARN.
		{"reverse_public_allzero_v4", true, false, "0.0.0.0:8180", true},
		{"reverse_public_allzero_v6", true, false, "[::]:8180", true},
		{"reverse_public_bareport", true, false, ":8180", true},
		{"reverse_public_lan_ip", true, false, "192.168.1.5:8180", true},
		{"reverse_malformed_addr_warns", true, false, "not_a_valid_addr", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := shouldWarnReverseNodePlaintext(tc.reverseServerEnabled, tc.trustedProxy, tc.addr)
			if got != tc.want {
				t.Errorf("shouldWarnReverseNodePlaintext(enabled=%v, trustedProxy=%v, addr=%q) = %v, want %v",
					tc.reverseServerEnabled, tc.trustedProxy, tc.addr, got, tc.want)
			}
		})
	}
}

// TestReverseNodePlaintextWarning_MentionsConcreteRisk regression-locks the
// warning text so a future rewrite cannot silently drop the specific threat
// enumeration. The value of this warning is that an operator scrolling a
// startup log can act immediately without a docs lookup, which requires the
// message to name (a) what is exposed, (b) the concrete attacker capability,
// and (c) two remediation paths. A rewrite must keep every fragment below or
// update this test deliberately in the same commit.
func TestReverseNodePlaintextWarning_MentionsConcreteRisk(t *testing.T) {
	t.Parallel()
	mustContain := []string{
		"/ws-node",
		"plaintext",
		"trusted proxy",
		"token",
		"impersonate",
		"trusted_proxy",
		"127.0.0.1",
	}
	for _, sub := range mustContain {
		if !strings.Contains(reverseNodePlaintextWarning, sub) {
			t.Errorf("reverseNodePlaintextWarning missing %q\n--- full text:\n%s", sub, reverseNodePlaintextWarning)
		}
	}
}
