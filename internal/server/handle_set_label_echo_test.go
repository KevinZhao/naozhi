package server

import (
	"os"
	"regexp"
	"testing"
)

// TestHandleSetLabel_BothPathsUseWriteOK pins the Round 170 XSS policy
// one-off: both the local-node and remote-node handleSetLabel arms must
// call writeOK(w), not writeJSON with the label echoed back. Echoing
// attacker-influenced text is a latent reflected-XSS vector if any
// future caller renders the response via innerHTML.
//
// Enforced as a source-level test because the behavioural equivalent
// would require spinning up a Router + httptest round-trip for both
// the happy and remote-forwarding paths, which duplicates
// dashboard_session_test coverage. Source-level is cheaper and more
// targeted at the specific regression.
func TestHandleSetLabel_BothPathsUseWriteOK(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("dashboard_session.go")
	if err != nil {
		t.Fatalf("read dashboard_session.go: %v", err)
	}
	text := string(src)

	// Forbid the legacy `writeJSON(w, map[string]string{"status": "ok", "label": label})`
	// shape in the handleSetLabel region. We match the specific pre-fix
	// literal to avoid false positives from unrelated writeJSON calls.
	legacy := regexp.MustCompile(`writeJSON\(w,\s*map\[string\]string\{"status":\s*"ok",\s*"label":\s*label\}\)`)
	if legacy.MatchString(text) {
		t.Error("handleSetLabel still contains legacy `writeJSON(... \"label\": label)` echo — " +
			"replace with writeOK(w) so the attacker-influenced label is not reflected in the HTTP body. " +
			"The remote path's comment (dashboard_session.go, before writeOK(w) call) already documents why.")
	}
}
