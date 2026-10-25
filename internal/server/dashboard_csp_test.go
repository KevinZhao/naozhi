package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
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
	// Today every <script>/<link> render_md.js injects (the vendored mermaid
	// and KaTeX) already declares `integrity=`; the directive is therefore a
	// no-op for naozhi but locks the contract so a future change adding a
	// lazy asset without SRI fails closed when any browser revives the
	// withdrawn spec. Pin both `script` and `style` tokens (the original
	// directive only listed `font`).
	if !strings.Contains(csp, "require-sri-for script style font") {
		t.Errorf("CSP must include `require-sri-for script style font` as forward-compat SRI gate (R243-SEC-4 / R244-SEC-P2-4), got %q", csp)
	}
}

// TestDashboardCSP_StaticHandlersWiredInJS pins the R249-SEC-9 (#922) migration
// of the static header/sidebar inline handlers to addEventListener binds. The
// HTML no longer carries `onclick=`/`onsubmit=` for these controls (asserted by
// TestDashboardBundle_NoInlineHandlerAttributes); this test
// asserts the *replacement* wiring exists in dashboard.js so the controls stay
// functional. Without these binds the buttons would be inert — a silent
// behaviour regression that a CSP-only test would not catch.
//
// We assert two things per control:
//   - the element id is referenced by an addEventListener bind in dashboard.js
//   - the handler function it used to call inline is still defined
//
// Substring checks (not full parse) keep the test robust against whitespace /
// reordering while still failing loudly if a future edit drops a bind.
func TestDashboardCSP_StaticHandlersWiredInJS(t *testing.T) {
	t.Parallel()

	html := string(rawDashboardHTML(t))
	// Handler functions and binds may live in any bundle module.
	js := readDashboardJS(t)

	// Each migrated control: the element id that must exist in the HTML, and
	// the handler function name that must still be defined in the dashboard JS.
	controls := []struct {
		id      string
		handler string
	}{
		{"btn-history", "toggleHistory"},
		{"btn-new-session", "createNewSession"},
		{"btn-sidebar-toggle", "toggleSidebarCollapsed"},
		{"quick-ask-form", "submitQuickAsk"},
	}
	for _, c := range controls {
		if !strings.Contains(html, `id="`+c.id+`"`) {
			t.Errorf("dashboard.html missing id=%q — the addEventListener bind in dashboard.js "+
				"can no longer find the migrated control (#922)", c.id)
		}
		if !strings.Contains(js, `getElementById('`+c.id+`')`) &&
			!strings.Contains(js, `bindClick('`+c.id+`'`) {
			t.Errorf("dashboard.js no longer references id %q via getElementById/bindClick — "+
				"the inline handler was removed from HTML (#922) but the replacement "+
				"addEventListener bind is missing, so the control is inert", c.id)
		}
		if !strings.Contains(js, "function "+c.handler+"(") {
			t.Errorf("dashboard JS missing handler function %q that the migrated control invokes (#922)", c.handler)
		}
	}
}

// inlineHandlerAttrRe matches inline event-handler ATTRIBUTES (`onclick="…"`,
// `onkeydown='…'`, incl. the escaped-quote form used inside JS template
// strings). Element property assignments (`btn.onclick = fn`) are CSP-legal
// and deliberately not matched — the preceding dot fails the \b…= shape here
// because we require the quote right after `=`.
var inlineHandlerAttrRe = regexp.MustCompile(`\bon[a-z]+\s*=\s*\\?["']`)

// TestDashboardBundle_NoInlineHandlerAttributes is the terminal gate of the
// #1980 migration: the whole dashboard bundle (static HTML + every JS file
// that emits innerHTML) carries ZERO inline event-handler attributes, for
// every on* type. Handlers are wired through the nz.actions data-action
// delegation; the CSP has no script-src 'unsafe-inline' to execute an inline
// attribute anyway, so any new one is a silently dead control.
func TestDashboardBundle_NoInlineHandlerAttributes(t *testing.T) {
	t.Parallel()
	files := append([]string{"dashboard.html"}, dashboardModules(t)...)
	for _, name := range files {
		data := staticAssetBytes(name)
		if data == nil {
			t.Fatalf("%s not embedded", name)
		}
		for _, m := range inlineHandlerAttrRe.FindAllStringIndex(string(data), -1) {
			line := 1 + strings.Count(string(data)[:m[0]], "\n")
			t.Errorf("%s:%d: inline event-handler attribute %q — wire it through "+
				"nz.actions data-action delegation instead (#1980; the CSP would "+
				"block it silently)", name, line, string(data)[m[0]:m[1]])
		}
	}
}

// TestDashboardCSP_MockServerHeaderInSync compares the Playwright mock
// server's hard-coded CSP literal against the policy the server derives for
// the page the mock serves, so the e2e suite always exercises the dashboard
// under the production policy. The literal lives in test/e2e/mock-server.js
// (MOCK_DASHBOARD_CSP); this test is the drift alarm the mock comment points at.
func TestDashboardCSP_MockServerHeaderInSync(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "test", "e2e", "mock-server.js"))
	if err != nil {
		t.Fatalf("read mock-server.js: %v", err)
	}
	m := regexp.MustCompile(`MOCK_DASHBOARD_CSP =\n  "([^"]+)";`).FindSubmatch(data)
	if m == nil {
		t.Fatal("mock-server.js: MOCK_DASHBOARD_CSP literal not found (regex drift?)")
	}
	if want := buildDashboardCSP(rawDashboardHTML(t)); string(m[1]) != want {
		t.Errorf("mock-server.js MOCK_DASHBOARD_CSP is out of sync with the runtime header\nmock: %s\ngo:   %s", m[1], want)
	}
}
