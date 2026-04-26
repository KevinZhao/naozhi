package server

import (
	"strings"
	"testing"
)

// TestShouldWarnNoTokenOpen_Matrix locks the decision matrix for the
// R60-SEC-006 / R70-SEC-M1 "no dashboard_token on a public bind" warning.
// Four axes produce a small but security-critical truth table: token
// set/unset × loopback/public bind × trustedProxy true/false. A silent
// regression that suppresses the warning (e.g. inverted condition, accidental
// loopback classification of a public IP) would let an open control plane
// ship without the operator seeing the startup journal line.
func TestShouldWarnNoTokenOpen_Matrix(t *testing.T) {
	cases := []struct {
		name         string
		token        string
		addr         string
		trustedProxy bool
		want         bool
	}{
		// Token set: caller has credentials, never warn (operator opted in).
		{"token_set_loopback", "s3cret", "127.0.0.1:8180", false, false},
		{"token_set_public", "s3cret", "0.0.0.0:8180", false, false},
		{"token_set_public_proxied", "s3cret", "0.0.0.0:8180", true, false},

		// Token empty: only warn when public AND not behind trusted proxy.
		{"no_token_loopback_v4", "", "127.0.0.1:8180", false, false},
		{"no_token_loopback_v6", "", "[::1]:8180", false, false},
		{"no_token_loopback_name", "", "localhost:8180", false, false},
		{"no_token_public_proxied", "", "0.0.0.0:8180", true, false},
		{"no_token_public_allzero_v4", "", "0.0.0.0:8180", false, true},
		{"no_token_public_allzero_v6", "", "[::]:8180", false, true},
		{"no_token_public_bareport", "", ":8180", false, true},
		{"no_token_public_lan_ip", "", "192.168.1.5:8180", false, true},
		{"no_token_malformed_addr_warns", "", "not_a_valid_addr", false, true}, // err-on-side-of-visibility
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldWarnNoTokenOpen(tc.token, tc.addr, tc.trustedProxy)
			if got != tc.want {
				t.Errorf("shouldWarnNoTokenOpen(%q, %q, %v) = %v, want %v",
					tc.token, tc.addr, tc.trustedProxy, got, tc.want)
			}
		})
	}
}

// TestNoTokenOpenWarning_MentionsAPIRisk regression-locks the warning text
// to ensure a future operator-facing rewrite does not silently drop the
// concrete risk enumeration — the value of this warning is that an operator
// who sees it in the journal can act immediately without a docs roundtrip,
// and that requires the text to name the specific capabilities exposed.
// If the message is restructured, every listed risk term below must still
// be present; tests should be updated IN PARALLEL with the user-facing copy.
func TestNoTokenOpenWarning_MentionsAPIRisk(t *testing.T) {
	// The warning MUST call out that the entire API is open — not just the
	// per-subsystem warnings it supersedes. These substrings ensure no one
	// accidentally trims the message back to "uploadOwner falls back" (the
	// pre-R60-SEC-006 state that failed to flag the broader control-plane
	// exposure).
	mustContain := []string{
		"no dashboard_token",
		"non-loopback",
		"ENTIRE dashboard API",
		"send messages",
		"workspace files",
		"cron schedules",
		"uploadOwner",
		"127.0.0.1",
		"trusted_proxy",
	}
	for _, sub := range mustContain {
		if !strings.Contains(noTokenOpenWarning, sub) {
			t.Errorf("noTokenOpenWarning missing %q\n--- full text:\n%s", sub, noTokenOpenWarning)
		}
	}
}
