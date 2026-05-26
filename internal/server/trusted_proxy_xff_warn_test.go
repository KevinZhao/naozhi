package server

import (
	"strings"
	"testing"
)

// TestTrustedProxyXFFReminder_MentionsConcreteRisk regression-locks the
// startup info-level reminder emitted whenever trusted_proxy=true.
// R238-SEC-15 (#848): per-IP rate limiters, audit-log client_ip fields,
// and the same-origin gate all derive their key from the last
// X-Forwarded-For hop once trusted_proxy=true. If the upstream proxy
// fails to strip client-supplied XFF headers (or honours unbounded XFF
// depth), an attacker can spoof the rate-limit key by prepending a
// victim's IP to the header — a silent misconfiguration the process
// itself cannot detect.
//
// The value of this reminder is that an operator scrolling a startup
// journal sees the upstream contract requirement explicitly, without
// a docs lookup. Any future rewrite must preserve every fragment
// below (which name the affected gates, the concrete attack, and the
// fix surfaces) or update this test in the same commit. A regression
// that drops the reminder entirely would also fail this test by
// surfacing an empty-string mismatch on the first .Contains call.
func TestTrustedProxyXFFReminder_MentionsConcreteRisk(t *testing.T) {
	t.Parallel()
	mustContain := []string{
		"trusted_proxy=true",
		"X-Forwarded-For",
		"per-IP",
		"strip",          // operator action: drop client-supplied XFF
		"hop-count",      // alternative mitigation
		"upstream proxy", // where the fix must live
		"spoofed",        // names the concrete attack vector
	}
	for _, sub := range mustContain {
		if !strings.Contains(trustedProxyXFFReminder, sub) {
			t.Errorf("trustedProxyXFFReminder missing %q\n--- full text:\n%s", sub, trustedProxyXFFReminder)
		}
	}
}
