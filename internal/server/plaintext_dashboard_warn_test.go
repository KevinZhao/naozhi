package server

import (
	"strings"
	"testing"
)

// TestPlaintextDashboardTokenWarning_MentionsHealthLeak regression-locks the
// R217-SEC-8 (#602) warning text. The previous message only mentioned bearer
// tokens / session cookies; the issue calls out that even when the operator
// sets a token, an authenticated /health response over plaintext leaks
// workspace_id / node status / version / watchdog fields to a passive
// sniffer who already has the cookie. The warning must spell that out so an
// operator scrolling the startup journal sees the full attack surface in one
// line — without needing to crawl the source for the auth-section schema.
//
// If a future operator-facing rewrite restructures the text, every listed
// risk term below must still be present; tests should be updated IN PARALLEL
// with the user-facing copy.
func TestPlaintextDashboardTokenWarning_MentionsHealthLeak(t *testing.T) {
	t.Parallel()
	mustContain := []string{
		"dashboard token served over plaintext",
		"trusted proxy",
		"bearer tokens",
		"session cookies",
		// R217-SEC-8 (#602) additions: spell out /health as a concrete
		// passive-sniff target so the operator does not assume token+
		// auth=safe under plaintext.
		"/health",
		"workspace_id",
		"node status",
		"version",
		"watchdog",
		// Remediation knobs the operator can flip without docs lookup.
		"server.trusted_proxy=true",
		"127.0.0.1",
	}
	for _, sub := range mustContain {
		if !strings.Contains(plaintextDashboardTokenWarning, sub) {
			t.Errorf("plaintextDashboardTokenWarning missing %q\n--- full text:\n%s",
				sub, plaintextDashboardTokenWarning)
		}
	}
}
