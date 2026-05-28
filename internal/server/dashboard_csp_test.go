package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDashboardCSP_ConnectSrcSelfOnly locks the R243-SEC / R249-SEC posture
// against the H2 / #441 companion ask: do NOT relax `connect-src 'self'` to
// `connect-src 'self' wss:`. The companion proposal in #441 reads "tighten"
// but listing `wss:` actually *widens* the directive — `'self'` already
// covers the same-origin ws:// + wss:// upgrade implicitly (browsers pick
// the scheme to match the page), while explicit `wss:` accepts WebSocket
// connections to **any** origin. That re-opens an XS-Leak / data-exfil
// channel for any future DOM-XSS that lands on the dashboard.
//
// We pin this explicitly so a reviewer following #441's wording cannot
// silently land the over-permissive form. The exact substring is the only
// load-bearing assertion; the directive ordering inside the header value
// is irrelevant for browser CSP parsing but the substring lookup keeps
// the test robust against unrelated reorderings.
func TestDashboardCSP_ConnectSrcSelfOnly(t *testing.T) {
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

	// Must still carry connect-src 'self' (otherwise default-src kicks in,
	// and a future default-src tweak would silently widen the connect
	// surface).
	if !strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("CSP must carry connect-src 'self' so same-origin ws/wss are accepted via the implicit scheme match, got %q", csp)
	}

	// Must NOT widen connect-src to listing wss: (or ws:) explicitly.
	// Either of those tokens accepts cross-origin WebSocket endpoints,
	// which is exactly the XS-Leak / exfil hole that a future DOM-XSS
	// could ride out of the dashboard origin. #441 / R249 / R243 / R236
	// — wording in the proposal reads "tighten" but the proposed change
	// is a widening; pin the safer form here.
	for _, bad := range []string{
		"connect-src 'self' wss:",
		"connect-src 'self' ws:",
		"connect-src 'self' ws: wss:",
		"connect-src 'self' wss: ws:",
	} {
		if strings.Contains(csp, bad) {
			t.Errorf("CSP must not widen connect-src to explicit wss:/ws: scheme listing — that lets cross-origin WebSocket endpoints accept dashboard data exfil. got %q (matched %q)", csp, bad)
		}
	}
}

// TestDashboardCSP_FrameSrcBlob locks the regression that broke workspace .html
// preview: dashboard.js renderSandboxedBlob fetches workspace HTML, wraps the
// bytes in a Blob({type:'text/html'}), and points a sandboxed iframe at the
// resulting blob: URL. The page-level CSP must list `blob:` under frame-src
// (or fall through to a default-src that does) — `'self'` does not match the
// blob: scheme. Without this directive the browser blocks iframe.src=blob:...
// loading and the preview drawer stays blank.
//
// The `sandbox=""` attribute on the iframe still grants zero capabilities
// regardless of frame-src, and serveRender continues to return
// application/octet-stream + attachment to neuter direct-URL navigation, so
// allowing blob: in frame-src does not regress the existing three-layer
// defense (octet-stream / opaque blob origin / iframe sandbox).
func TestDashboardCSP_FrameSrcBlob(t *testing.T) {
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

	// frame-src must allow blob: so renderSandboxedBlob can iframe a blob:
	// URL constructed from a fetched workspace .html. CSP `'self'` alone
	// does not match the blob: scheme.
	if !strings.Contains(csp, "frame-src") {
		t.Errorf("CSP missing frame-src directive (workspace .html preview will be blocked), got %q", csp)
	}
	// Look for blob: appearing in any frame-src / child-src / default-src
	// fallback. A simple substring check is fine because the directive list
	// is fixed in handleDashboard and we do not want to accept an
	// accidentally over-permissive value either.
	if !strings.Contains(csp, "frame-src 'self' blob:") {
		t.Errorf("CSP frame-src must explicitly allow 'self' + blob: for renderSandboxedBlob, got %q", csp)
	}

	// Defense-in-depth: img-src already allows blob: (used by composer image
	// previews). Lock that contract too so a future CSP refactor that
	// removes blob: from img-src does not silently break the upload path.
	if !strings.Contains(csp, "img-src 'self' data: blob:") {
		t.Errorf("CSP img-src must keep `data: blob:` for composer image previews, got %q", csp)
	}

	// R243-SEC-4 / R244-SEC-P2-4 [REPEAT-3]: dashboard CSP must carry
	// `require-sri-for script style font` as a forward-compatibility hook.
	// Today every CDN <script>/<link> injected by dashboard.js (mermaid,
	// KaTeX) already declares `integrity=`; the directive is therefore a
	// no-op for naozhi but locks the contract so a future change adding a
	// CDN asset without SRI fails closed when any browser revives the
	// withdrawn spec. Pin both `script` and `style` tokens (the original
	// directive only listed `font`).
	if !strings.Contains(csp, "require-sri-for script style font") {
		t.Errorf("CSP must include `require-sri-for script style font` as forward-compat SRI gate (R243-SEC-4 / R244-SEC-P2-4), got %q", csp)
	}
}
