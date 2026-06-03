package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDashboardCSP_KatexFontSRIForwardCompat pins R247-SEC-23 / R246-SEC-10
// (#518): the dashboard CSP must keep the `font` token inside its
// `require-sri-for` directive so a future CDN compromise of the KaTeX
// woff2 bundle on cdn.jsdelivr.net cannot land an attacker-substituted
// font into the page.
//
// Why this is a separate test from TestDashboardCSP_FrameSrcBlob: the
// frame-src test already asserts the full `require-sri-for script style
// font` substring and would catch a literal removal, but it folds three
// concerns (font / script / style) into one error message. A reviewer
// chasing issue #518 (font supply-chain) deserves a focused failure
// message that names the issue and the threat model — otherwise the
// next contributor refactoring CSP can drop the `font` token, satisfy
// the broader pin by replacing it with `require-sri-for script style`,
// and silently re-introduce the vulnerability the broader test was
// supposed to prevent.
//
// SRI on @font-face is *not* currently enforceable by any shipping
// browser (the once-proposed CSP3 directive was withdrawn before
// implementation), so this is a forward-compatibility hook: the day a
// vendor revives the proposal, we get integrity enforcement for free.
// Vendoring KaTeX fonts via `//go:embed` is the eventual mitigation but
// is tracked separately in dashboard.go's NEEDS-DESIGN comment because
// it requires bundling ~6 MB of woff2 + a CSS rewriter.
func TestDashboardCSP_KatexFontSRIForwardCompat(t *testing.T) {
	s := newTestServer(&mockPlatform{})

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()
	s.handleDashboard(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	csp := w.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("Content-Security-Policy header missing on /dashboard")
	}

	// The font-src directive must continue to allow cdn.jsdelivr.net so
	// KaTeX renders math; fully removing it is the //go:embed vendoring
	// work tracked NEEDS-DESIGN in routes.go, not this test's scope. But
	// the allowance MUST stay narrowed to the `/npm/` path prefix
	// (R242-SEC-2 / #607): a bare `https://cdn.jsdelivr.net` host-source
	// would let a CDN compromise serve attacker-substituted woff2 from any
	// jsdelivr path (e.g. `/gh/<attacker>/<repo>`), which is the exact
	// supply-chain surface #518 narrows. Pin the `/npm/`-scoped form so a
	// future CSP refactor cannot silently widen font-src back to the whole
	// CDN host.
	if !strings.Contains(csp, "font-src 'self' https://cdn.jsdelivr.net/npm/") {
		t.Errorf("CSP font-src must keep the /npm/-scoped cdn.jsdelivr.net "+
			"allowance for KaTeX woff2 (R247-SEC-23 / R242-SEC-2 / #518), got %q", csp)
	}

	// require-sri-for must contain the `font` token. Ordering inside the
	// directive value is meaningful per the spec (whitespace-separated
	// tokens), so check for the literal token rather than the broader
	// substring used by TestDashboardCSP_FrameSrcBlob.
	idx := strings.Index(csp, "require-sri-for")
	if idx < 0 {
		t.Fatalf("CSP must declare require-sri-for (#518), got %q", csp)
	}
	tail := csp[idx+len("require-sri-for"):]
	// Stop at the next `;` so a directive ordering change doesn't pull
	// tokens from the next CSP directive into the comparison.
	if semi := strings.IndexByte(tail, ';'); semi >= 0 {
		tail = tail[:semi]
	}
	tokens := strings.Fields(tail)
	hasFont := false
	for _, tok := range tokens {
		if tok == "font" {
			hasFont = true
			break
		}
	}
	if !hasFont {
		t.Errorf("require-sri-for must include the `font` token to forward-protect "+
			"KaTeX woff2 from cdn.jsdelivr.net compromise (R247-SEC-23 / #518), "+
			"got tokens=%v", tokens)
	}
}
