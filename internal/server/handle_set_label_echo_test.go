package server

import (
	"regexp"
	"strings"
	"testing"
)

// TestHandleSetLabel_BothPathsUseWriteOK pins the Round 170 XSS policy
// one-off: both the local-node and remote-node HandleSetLabel arms must
// call httputil.WriteOK(w), not a JSON write with the label echoed back.
// Echoing attacker-influenced text is a latent reflected-XSS vector if any
// future caller renders the response via innerHTML.
//
// Enforced as a source-level test because the behavioural equivalent
// would require spinning up a Router + httptest round-trip for both
// the happy and remote-forwarding paths, which duplicates
// dashboard_session_test coverage. Source-level is cheaper and more
// targeted at the specific regression.
//
// The test reads the whole internal/dashboard/session package rather than
// a single file by name: #2627 moved HandleSetLabel from handlers.go to
// mutations.go and this test kept reading handlers.go, so its "must NOT
// contain" assertion passed vacuously for five merges (#2630). The
// positive anchor below makes a future move fail loudly instead.
func TestHandleSetLabel_BothPathsUseWriteOK(t *testing.T) {
	t.Parallel()
	pkg := readDashboardSessionSource(t)

	// Positive anchor: the handler must exist somewhere in the package,
	// otherwise the negative assertion below checks nothing.
	const decl = "func (h *Handlers) HandleSetLabel("
	start := strings.Index(pkg, decl)
	if start < 0 {
		t.Fatalf("HandleSetLabel not found in internal/dashboard/session — "+
			"if the handler was renamed or moved out of the package, update this test's anchor %q", decl)
	}
	// Scope to the function body: up to the next top-level func decl (or EOF).
	body := pkg[start:]
	if next := strings.Index(body[len(decl):], "\nfunc "); next >= 0 {
		body = body[:len(decl)+next]
	}

	// Second positive anchor: the fix itself must still be present. Both
	// arms (local + remote-forward) end in WriteOK, so at least two calls.
	if n := strings.Count(body, "httputil.WriteOK(w)"); n < 2 {
		t.Errorf("HandleSetLabel body has %d httputil.WriteOK(w) call(s), want >= 2 (local + remote arms):\n%s", n, body)
	}

	// Negative: forbid the legacy `writeJSON(w, map[string]string{"status": "ok", "label": label})`
	// shape (and its httputil.WriteJSON spelling) inside HandleSetLabel.
	legacy := regexp.MustCompile(`[wW]riteJSON\(w,\s*map\[string\]string\{"status":\s*"ok",\s*"label":\s*label\}\)`)
	if legacy.MatchString(body) {
		t.Error("HandleSetLabel still contains legacy `writeJSON(... \"label\": label)` echo — " +
			"replace with httputil.WriteOK(w) so the attacker-influenced label is not reflected in the HTTP body.")
	}
	// Broader negative: no JSON body at all may carry the label back.
	if regexp.MustCompile(`"label":\s*label\b`).MatchString(body) {
		t.Error("HandleSetLabel echoes the request label in a response body — the label is attacker-influenced text and must not be reflected")
	}
}
