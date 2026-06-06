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

	// Cap on `onclick=` attributes. R249-SEC-9 (#922) migration: the static
	// HTML's header/sidebar handlers (sidebar-search, history, new-session,
	// cron, sidebar-search-clear, ns-trigger, sidebar-toggle) plus the
	// quick-ask form's onsubmit were moved into dashboard.js as
	// addEventListener binds (DOMContentLoaded header binder + wireQuickAskInput
	// for the repaint-prone quick-ask form), driving the static surface to 0.
	// The cap is now 0: any new inline `onclick=` in the static HTML is a
	// regression that pushes the script-src 'unsafe-inline' migration backwards
	// and must instead be wired via addEventListener.
	//
	// NOTE: this caps the STATIC dashboard.html only. dashboard.js still emits
	// inline `onclick=` attributes inside innerHTML template strings for
	// dynamically-rendered controls (session cards, project rows, history
	// items, etc.); those keep script-src 'unsafe-inline' required and remain
	// the NEEDS-DESIGN event-delegation rewrite tracked on #922 / #441 / #479.
	const inlineOnclickCap = 0
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
	// `onsubmit=` is included now that the quick-ask form was migrated to an
	// addEventListener('submit', …) bind (#922) — re-introducing it would
	// push the script-src migration backwards just like a new onclick=.
	for _, attr := range []string{"onload=", "onerror=", "onfocus=", "onmouseover=", "onsubmit="} {
		if strings.Contains(html, attr) {
			t.Errorf("static/dashboard.html contains inline `%s` handler — "+
				"these auto-fire and are higher-risk than onclick (R236-SEC-02 #479)", attr)
		}
	}
}

// TestDashboardCSP_StaticHandlersWiredInJS pins the R249-SEC-9 (#922) migration
// of the static header/sidebar inline handlers to addEventListener binds. The
// HTML no longer carries `onclick=`/`onsubmit=` for these controls (asserted by
// the cap=0 in TestDashboardCSP_InlineHandlerSurfaceDoesNotGrow); this test
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

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Join(filepath.Dir(self), "static")

	htmlBytes, err := os.ReadFile(filepath.Join(dir, "dashboard.html"))
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(htmlBytes)

	jsBytes, err := os.ReadFile(filepath.Join(dir, "dashboard.js"))
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(jsBytes)

	// Each migrated control: the element id that must exist in the HTML, and
	// the handler function name that must still be defined in dashboard.js.
	controls := []struct {
		id      string
		handler string
	}{
		{"btn-sidebar-search", "toggleSidebarSearch"},
		{"btn-history", "toggleHistory"},
		{"btn-new-session", "createNewSession"},
		{"btn-cron", "openCronPanel"},
		{"sidebar-search-clear", "closeSidebarSearch"},
		{"ns-trigger", "toggleNodeSelector"},
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
			t.Errorf("dashboard.js missing handler function %q that the migrated control invokes (#922)", c.handler)
		}
	}
}

// TestDashboardCSP_ScriptSrcUnsafeInlineMigrationGate pins the R20260531A-SEC-10
// (#1526) single-point invariant on the `script-src 'unsafe-inline'` posture.
// Modelled on TestSetEscapeHTMLFalseScopedToPackage: lock the *current* state so
// a refactor cannot silently land the half-migrated, worst-of-both-worlds form.
//
// Two coupled invariants:
//
//  1. script-src today still carries `'unsafe-inline'`. The dashboard ships a
//     fixed set of inline `onclick=` handlers (see
//     TestDashboardCSP_InlineHandlerSurfaceDoesNotGrow), so dropping
//     `'unsafe-inline'` *without* first migrating those handlers to
//     addEventListener would brick every header button. This asserts the
//     directive is present so an accidental removal fails loudly.
//
//  2. script-src does NOT yet carry `nonce-` or `strict-dynamic`. The full
//     migration is NEEDS-DESIGN (#441 / #479 / #922). Crucially, per CSP3
//     browsers *ignore* `'unsafe-inline'` once `nonce-`/`strict-dynamic` is
//     present — so landing a nonce alongside the still-present `'unsafe-inline'`
//     would silently disable the inline handlers (a functional break) while
//     looking "more secure". This gate forces the nonce migration to drop
//     `'unsafe-inline'` in the *same* change: adding nonce/strict-dynamic trips
//     invariant (1)'s sibling failure here until `'unsafe-inline'` is removed,
//     making the two changes atomic.
//
// This test deliberately does NOT remove `'unsafe-inline'` — that is the
// NEEDS-DESIGN migration. It only fences the current state against silent drift.
func TestDashboardCSP_ScriptSrcUnsafeInlineMigrationGate(t *testing.T) {
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

	// Isolate the script-src directive so the assertions below cannot be
	// satisfied (or tripped) by tokens that live in style-src / img-src / etc.
	var scriptSrc string
	for _, dir := range strings.Split(csp, ";") {
		dir = strings.TrimSpace(dir)
		if strings.HasPrefix(dir, "script-src ") || dir == "script-src" {
			scriptSrc = dir
			break
		}
	}
	if scriptSrc == "" {
		t.Fatalf("CSP missing script-src directive, got %q", csp)
	}

	// Invariant (1): current state ships 'unsafe-inline'.
	if !strings.Contains(scriptSrc, "'unsafe-inline'") {
		t.Errorf("R20260531A-SEC-10 (#1526): script-src dropped `'unsafe-inline'` while the "+
			"dashboard still ships inline onclick= handlers — every header button breaks. "+
			"Removing it must be bundled with migrating those handlers to addEventListener "+
			"(#441 / #479 / #922). got script-src %q", scriptSrc)
	}

	// Invariant (2): nonce/strict-dynamic must NOT coexist with 'unsafe-inline'.
	// Their presence here means a partial migration landed that silently
	// disables the inline handlers (CSP3 ignores 'unsafe-inline' once a nonce
	// or strict-dynamic appears). The migration must drop 'unsafe-inline' in
	// the same change.
	for _, tok := range []string{"'nonce-", "'strict-dynamic'", "nonce-"} {
		if strings.Contains(scriptSrc, tok) {
			t.Errorf("R20260531A-SEC-10 (#1526): script-src introduced %q while still listing "+
				"`'unsafe-inline'` — per CSP3 the browser now ignores `'unsafe-inline'`, "+
				"silently breaking the dashboard's inline onclick handlers. The nonce/"+
				"strict-dynamic migration MUST remove `'unsafe-inline'` (and migrate the "+
				"inline handlers) in the same change. got script-src %q", tok, scriptSrc)
		}
	}
}

// generatedOnclickCap is the downward-only ratchet on the number of inline
// `onclick=` attributes that dashboard.js emits into innerHTML template strings
// for dynamically-rendered controls (session cards, project rows, cron cards,
// history items, etc.). Unlike the STATIC dashboard.html surface (capped at 0
// by TestDashboardCSP_InlineHandlerSurfaceDoesNotGrow), these JS-generated
// handlers are the remaining reason script-src still needs `'unsafe-inline'`
// (the NEEDS-DESIGN bundle on #441 / #479 / #922; phased landing tracked on
// #1734).
//
// This cap is a RATCHET: it must only ever be LOWERED. Each migration PR that
// converts a batch of `onclick="fn(args)"` emissions to the existing
// data-action dispatch idiom (see SIDEBAR_PROJECT_ACTIONS / CRON_MENU_ACTIONS
// in dashboard.js) must drop this number to the new post-migration count in the
// SAME change, so the test stays green and pins the reduction. Raising it is a
// regression that pushes the script-src 'unsafe-inline' removal backwards and
// must be rejected — add a data-action dispatch entry instead.
const generatedOnclickCap = 84

// TestDashboardCSP_GeneratedHandlerSurfaceRatchet pins the JS-generated inline
// `onclick=` surface in static/dashboard.js as a downward-only ratchet
// (#922 / #1734). The static-HTML inline-handler surface is already at 0
// (TestDashboardCSP_InlineHandlerSurfaceDoesNotGrow); this test covers the
// harder, larger surface that dashboard.js still emits inside template strings.
//
// Mirrors the inlineOnclickCap=0 idiom in
// TestDashboardCSP_InlineHandlerSurfaceDoesNotGrow but against dashboard.js and
// with a non-zero cap that shrinks per migration PR. Driving the count to 0 is
// the precondition for dropping script-src 'unsafe-inline' (gated atomically by
// TestDashboardCSP_ScriptSrcUnsafeInlineMigrationGate).
//
// NOTE: the regexp counts every `onclick=` token in the file text, including
// any that appear in comments. Keep comment prose free of the literal
// `onclick=` token (write "inline click attributes" instead) so the ratchet
// tracks real emitted handlers, not documentation.
func TestDashboardCSP_GeneratedHandlerSurfaceRatchet(t *testing.T) {
	t.Parallel()

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	jsPath := filepath.Join(filepath.Dir(self), "static", "dashboard.js")
	body, err := os.ReadFile(jsPath)
	if err != nil {
		t.Fatalf("read %s: %v", jsPath, err)
	}
	js := string(body)

	onclickRe := regexp.MustCompile(`\bonclick\s*=`)
	got := len(onclickRe.FindAllStringIndex(js, -1))
	if got > generatedOnclickCap {
		t.Errorf("static/dashboard.js has %d inline `onclick=` attributes, ratchet cap is %d "+
			"(#922 / #1734). This cap is downward-only: convert a batch of generated "+
			"`onclick=\"fn(args)\"` emissions to the existing data-action dispatch "+
			"(SIDEBAR_PROJECT_ACTIONS / CRON_MENU_ACTIONS) and LOWER the cap in the same "+
			"change. Goal: drive the count to 0 so script-src 'unsafe-inline' can be dropped.",
			got, generatedOnclickCap)
	}
}
