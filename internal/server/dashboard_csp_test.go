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

// TestDashboardCSP_Smoke pins the policy /dashboard serves. script-src is
// 'self' plus the inline-script hashes, with no 'unsafe-inline' (a new inline
// handler is a dead control, not a live injection sink). No directive names an
// outside origin: KaTeX and mermaid are served from /static/vendor, and
// connect-src 'self' already covers the same-origin ws/wss upgrade, while an
// explicit wss: would accept any origin. frame-src and img-src keep blob: for
// the sandboxed workspace .html preview and composer image previews.
func TestDashboardCSP_Smoke(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	newTestServer(&mockPlatform{}).handleDashboard(w, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	csp := w.Header().Get("Content-Security-Policy")
	if csp != dashboardCSP {
		t.Fatalf("/dashboard serves CSP %q, want dashboardCSP", csp)
	}
	directives := map[string]string{}
	for _, d := range strings.Split(csp, ";") {
		f := strings.Fields(d)
		directives[f[0]] = strings.Join(f[1:], " ")
		for _, src := range f[1:] {
			switch {
			case src == "'self'", src == "'none'", src == "data:", src == "blob:":
			case src == "'unsafe-inline'" && f[0] == "style-src":
			case strings.HasPrefix(src, "'sha256-") && f[0] == "script-src":
			default:
				t.Errorf("CSP %s allows %q: the dashboard loads nothing from outside its origin", f[0], src)
			}
		}
	}
	for name, want := range map[string]string{
		"default-src":     "'self'",
		"connect-src":     "'self'",
		"style-src":       "'self' 'unsafe-inline'",
		"font-src":        "'self'",
		"img-src":         "'self' data: blob:",
		"frame-src":       "'self' blob:",
		"object-src":      "'none'",
		"base-uri":        "'none'",
		"form-action":     "'self'",
		"frame-ancestors": "'none'",
	} {
		if got := directives[name]; got != want {
			t.Errorf("CSP %s = %q, want %q", name, got, want)
		}
	}
	if script := directives["script-src"]; !strings.HasPrefix(script, "'self' 'sha256-") {
		t.Errorf("CSP script-src = %q, want 'self' and the inline-script hashes", script)
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
