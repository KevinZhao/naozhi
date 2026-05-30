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

// TestDashboardCSP_BaseURIAndFormAction pins the R242-SEC-1 / R249-SEC-9
// (#605, #922) interim hardening that lands ahead of the full nonce migration:
//   - base-uri 'none' stops an injected <base href> from re-rooting the
//     relative /static/*.js script tags at an attacker origin (script
//     substitution even within the existing script-src allowlist).
//   - form-action 'self' stops an injected form from POSTing dashboard data
//     to a foreign origin; the only real form submits via fetch + preventDefault.
//
// Both are additive (no behaviour change for the shipped page) so dropping
// either is a pure security regression — pin them here.
func TestDashboardCSP_BaseURIAndFormAction(t *testing.T) {
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

	if !strings.Contains(csp, "base-uri 'none'") {
		t.Errorf("CSP must carry base-uri 'none' (#605/#922): without it an injected "+
			"<base href> re-roots the relative /static/*.js script tags at an attacker "+
			"origin. got %q", csp)
	}
	if !strings.Contains(csp, "form-action 'self'") {
		t.Errorf("CSP must carry form-action 'self' (#605/#922): without it an injected "+
			"form can exfiltrate dashboard data to a foreign origin. got %q", csp)
	}
}

// TestDashboardCSP_ObjectSrcNone pins the R249-SEC-9 (#922) explicit plugin
// lockdown: object-src 'none' forbids <object>/<embed>/<applet>, a legacy
// script-execution vector that default-src 'self' would still permit for
// same-origin sources. The dashboard ships zero plugin elements, so the
// strict 'none' form is correct and dropping it is a pure regression.
func TestDashboardCSP_ObjectSrcNone(t *testing.T) {
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
	if !strings.Contains(csp, "object-src 'none'") {
		t.Errorf("CSP must carry object-src 'none' (#922): plugin embeds are a legacy "+
			"script-execution vector default-src 'self' does not lock down. got %q", csp)
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

// TestDashboardCSP_JsdelivrNpmPathScoped pins R242-SEC-2 (#607): every
// `cdn.jsdelivr.net` source expression in the dashboard CSP must carry the
// `/npm/` path prefix so the CDN scope can only load assets under the npm
// subtree (mermaid + KaTeX live there), not arbitrary follow-on resources
// from other jsdelivr path namespaces (`/gh/<attacker>/<repo>`, `/combine/`
// bundle endpoints, etc.). A bare `https://cdn.jsdelivr.net` host-source
// re-opens that surface, so the test fails if any directive lists the host
// without the `/npm/` path segment.
//
// CSP3 §6.6.2.6 path-part matching: a trailing-slash path in a host-source
// matches every URL whose path begins with that prefix, so all three shipped
// CDN URLs (`/npm/mermaid@…`, `/npm/katex@…/dist/katex.min.js`, the KaTeX
// stylesheet + its `/npm/katex@…/dist/fonts/*` woff2 files) still load.
func TestDashboardCSP_JsdelivrNpmPathScoped(t *testing.T) {
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

	// The CDN must only ever appear scoped to /npm/. Tokenise on whitespace
	// so a host-source that ends exactly at `cdn.jsdelivr.net` (no path) is
	// caught regardless of which directive it sits in.
	for _, tok := range strings.Fields(csp) {
		if !strings.Contains(tok, "cdn.jsdelivr.net") {
			continue
		}
		if !strings.HasPrefix(tok, "https://cdn.jsdelivr.net/npm/") {
			t.Errorf("R242-SEC-2 (#607): CSP source %q references cdn.jsdelivr.net "+
				"without the /npm/ path prefix — the CDN scope must be pinned to "+
				"https://cdn.jsdelivr.net/npm/ so a non-npm jsdelivr path cannot "+
				"bootstrap an arbitrary follow-on load. got CSP %q", tok, csp)
		}
	}

	// Positive: the three directives that legitimately pull from the CDN must
	// each carry the npm-scoped source.
	for _, want := range []string{
		"script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net/npm/",
		"style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net/npm/",
		"font-src 'self' https://cdn.jsdelivr.net/npm/",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("R242-SEC-2 (#607): CSP must carry %q so KaTeX/mermaid still "+
				"load from the npm-scoped CDN, got %q", want, csp)
		}
	}
}

// TestDashboardCSP_InlineHandlerSurfaceDoesNotGrow caps the inline event
// handler surface in static/dashboard.html (R236-SEC-02 / #479, also tracked
// as #922). The dashboard CSP still ships `script-src 'unsafe-inline'`
// because the static HTML contains a fixed set of `onclick=` attributes on
// header buttons (sidebar search, history, new session, cron panel,
// sidebar-search-clear, ns-trigger, sidebar-toggle resizer). Migrating to
// hash/nonce CSP requires moving those handlers into dashboard.js as
// addEventListener bindings.
//
// This test does NOT remove `'unsafe-inline'` (that is the NEEDS-DESIGN
// bundle on #441 / #479). Instead it pins the current surface count so a
// future feature that adds yet another inline handler trips a visible
// failure and the author has to consider whether to keep growing the
// 'unsafe-inline' justification. It also asserts no `onload=` /
// `onerror=` attributes exist (those are the more dangerous inline handler
// classes — neither is present today, so the gate is "stays at zero").
func TestDashboardCSP_InlineHandlerSurfaceDoesNotGrow(t *testing.T) {
	t.Parallel()

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	htmlPath := filepath.Join(filepath.Dir(self), "static", "dashboard.html")
	body, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatalf("read %s: %v", htmlPath, err)
	}
	html := string(body)

	// Cap on `onclick=` attributes. Current count is 7 (R236-SEC-02 audit
	// 2026-05-28). Assert ≤ 7 so the migration debt does not silently grow
	// while #479 sits in NEEDS-DESIGN. If a contributor needs to bump the
	// cap, the right move is to first migrate one of the existing handlers
	// to addEventListener — then bump the cap below as a no-op.
	const inlineOnclickCap = 7
	onclickRe := regexp.MustCompile(`\bonclick\s*=`)
	got := len(onclickRe.FindAllStringIndex(html, -1))
	if got > inlineOnclickCap {
		t.Errorf("R236-SEC-02 (#479): static/dashboard.html has %d inline `onclick=` attributes, "+
			"cap is %d. Migrate one to addEventListener in dashboard.js before adding more, "+
			"or this test bumps the cap as part of the same change. Goal: drive count to 0 "+
			"so script-src 'unsafe-inline' can be dropped (#441 / #479 / #922 bundle).",
			got, inlineOnclickCap)
	}

	// Stricter cap: dangerous inline handlers stay at zero. `onload` and
	// `onerror` on <img>/<script>/<iframe> tags can fire on any successful
	// load and are the easiest XSS vector even with `unsafe-inline`.
	for _, attr := range []string{"onload=", "onerror=", "onfocus=", "onmouseover="} {
		if strings.Contains(html, attr) {
			t.Errorf("static/dashboard.html contains inline `%s` handler — "+
				"these auto-fire and are higher-risk than onclick (R236-SEC-02 #479)", attr)
		}
	}
}
