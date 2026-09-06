package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

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
	files := append([]string{"dashboard.html"}, generatedOnclickBundle...)
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

// TestDashboardBundle_NoInterpolatedDataAction pins the injection hard rule
// from docs/rfc/csp-data-action.md: a data-action value must be a code
// literal — interpolating data into the action key would let an
// attribute-injection pick which registered handler runs. Parameters belong
// in sibling data-* attributes.
func TestDashboardBundle_NoInterpolatedDataAction(t *testing.T) {
	t.Parallel()
	// `data-action="' +` (and the -<type> variants) is the concatenation
	// shape; the cron menu's data-menu-action stays scoped to its own
	// listener and fixed item table, so it is exempt by attribute name.
	bad := regexp.MustCompile(`data-action[a-z-]*=\\?"' \+`)
	for _, name := range generatedOnclickBundle {
		data := staticAssetBytes(name)
		if data == nil {
			t.Fatalf("%s not embedded", name)
		}
		for _, m := range bad.FindAllStringIndex(string(data), -1) {
			line := 1 + strings.Count(string(data)[:m[0]], "\n")
			t.Errorf("%s:%d: data-action value built by string concatenation — "+
				"action keys must be code literals (parameters ride data-* "+
				"attributes)", name, line)
		}
	}
}

// TestDashboardCSP_CDNURLsMatchBundle keeps the exact-URL CDN pins in
// buildDashboardCSP in lockstep with the URLs dashboard.js actually injects:
// bumping mermaid/KaTeX in one place but not the other would either block the
// lazy load (CSP behind) or leave a stale allowlisted URL (CSP ahead).
func TestDashboardCSP_CDNURLsMatchBundle(t *testing.T) {
	t.Parallel()
	// #2558 D4: the lazy CDN loaders live in render_md.js; keep scanning
	// dashboard.js too so a future move back stays covered.
	var js []byte
	for _, name := range []string{"dashboard.js", "render_md.js"} {
		b := staticAssetBytes(name)
		if b == nil {
			t.Fatalf("%s not embedded", name)
		}
		js = append(append(js, b...), '\n')
	}
	urlRe := regexp.MustCompile(`https://cdn\.jsdelivr\.net/npm/[^'"\s]+`)
	seen := map[string]bool{}
	for _, u := range urlRe.FindAllString(string(js), -1) {
		seen[u] = true
		switch {
		case strings.HasSuffix(u, ".js"), strings.HasSuffix(u, ".css"):
			if !strings.Contains(dashboardCSP, u) {
				t.Errorf("dashboard.js injects %q but the CSP does not allowlist it — "+
					"update the cdn* constants in dashboard_csp.go in the same change", u)
			}
		}
	}
	for _, pinned := range []string{cdnMermaidJS, cdnKatexJS, cdnKatexCSS} {
		if !seen[pinned] {
			t.Errorf("CSP pins %q but dashboard.js no longer references it — drop or "+
				"update the pin", pinned)
		}
	}
}

// TestDashboardCSP_MockServerHeaderInSync compares the Playwright mock
// server's hard-coded CSP literal against the runtime header so the e2e
// suite always exercises the dashboard under the production policy. The
// literal lives in test/e2e/mock-server.js (MOCK_DASHBOARD_CSP); this test
// is the drift alarm the mock comment points at.
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
	if got := string(m[1]); got != dashboardCSP {
		t.Errorf("mock-server.js MOCK_DASHBOARD_CSP is out of sync with the runtime header\nmock: %s\ngo:   %s", got, dashboardCSP)
	}
}
