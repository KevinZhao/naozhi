package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestDashboardCSP_DataImgAuditPinned pins R236-SEC-14 (#562). The reported
// concern was that `img-src 'self' data: blob:` paired with
// `script-src ... 'unsafe-inline'` would expose a data: exfil channel for a
// future XSS injection. The audit outcome (recorded inline in
// handleDashboard's CSP comment) is:
//
//  1. `data:` cannot be removed today because three CSS rules in
//     static/dashboard.html (.cron-sort-select / .freq-mode-select /
//     .freq-extra) ship inline `data:image/svg+xml` background-image
//     URIs that the CSP spec routes through img-src; stripping data:
//     blanks out the cron / scheduler dropdown arrows.
//
//  2. The actual exfil precondition is an attacker-injected
//     `<img src="data:...">` element. The shipped HTML/JS contains zero
//     such occurrences. This test pins that absence so a future feature
//     that adds an img-tagged data: URI either (a) trips this assertion
//     and forces the author to consider the SEC-14 surface, or (b) lands
//     simultaneously with the bundled R236-SEC-02 (#441) `unsafe-inline`
//     removal that closes the channel.
//
// Bundle constraint: tightening img-src to `'self' blob:` must arrive
// together with replacing the dropdown-arrow data: URIs with
// /static-served SVG files AND removing `script-src 'unsafe-inline'`.
// All three are tracked together on #562 / #441 — this test is the
// audit-stable middle state.
func TestDashboardCSP_DataImgAuditPinned(t *testing.T) {
	t.Parallel()

	// Half 1: confirm the CSP shape stays at the audited middle state.
	// `data:` MUST stay in img-src (CSS dropdown arrows depend on it);
	// `'self'` and `blob:` MUST also stay (workspace image preview).
	s := newTestServer(&mockPlatform{})
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()
	s.handleDashboard(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "img-src 'self' data: blob:") {
		t.Errorf("dashboard CSP img-src must keep `'self' data: blob:` until R236-SEC-02 "+
			"(#441) unsafe-inline removal lands together with replacing CSS data: URIs — "+
			"got %q", csp)
	}

	// Half 2: confirm dashboard static assets do NOT contain `<img src="data:">`
	// — the actual SEC-14 exfil precondition. The CSS `background-image:
	// url("data:image/svg+xml,...")` rules are allowed (they're the reason
	// img-src must keep data:), but a literal <img src="data:..."> in the
	// HTML or JS templates would be an injection-amplifier and is flagged.
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(self)
	// Scope: dashboard.html only. JS files legitimately reference
	// `<img src="data:...">` in code comments that document why a
	// particular path AVOIDS injecting such a tag (see dashboard.js
	// paste-handler comment around line 4135). Restricting the scan to
	// HTML keeps the assertion a true exfil-precondition gate without
	// false-positive churn from explanatory prose. A JS path that
	// dynamically constructs `<img src="data:...">` would have to be
	// caught by the existing innerHTML / DOMPurify review — itself
	// covered by separate R238-SEC-5 / R245-SEC-13 contracts.
	staticFiles := []string{
		filepath.Join(dir, "static", "dashboard.html"),
	}
	// Match `<img ... src="data:` allowing arbitrary attribute order /
	// whitespace before src=. Multiline mode unnecessary because <img>
	// tags rarely span lines in shipped HTML; if a future contributor
	// prettifies the HTML and triggers a false negative, the failure is
	// "test stops catching the regression" (visible in code review),
	// not "test passes a real bug" (silent regression).
	imgDataRe := regexp.MustCompile(`<img\b[^>]*\bsrc\s*=\s*"data:`)
	for _, path := range staticFiles {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if loc := imgDataRe.FindIndex(body); loc != nil {
			// Slice a small context window for the failure message so
			// the reviewer sees what tripped the gate.
			start := loc[0] - 20
			if start < 0 {
				start = 0
			}
			end := loc[1] + 60
			if end > len(body) {
				end = len(body)
			}
			t.Errorf("R236-SEC-14 (#562): %s contains `<img src=\"data:...\">` "+
				"which together with `script-src 'unsafe-inline'` is the SEC-14 exfil "+
				"channel. Either (a) replace with /static-served SVG and remove img-src "+
				"data:, or (b) land #441 unsafe-inline removal first. Context: %q",
				path, string(body[start:end]))
		}
	}
}
