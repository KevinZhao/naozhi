// static_toplevel_views_contract_test.go — contract tests for the codex-style
// unified activity rail (会话 / 资产 / 自动化 / 设置) introduced when cron and
// settings were promoted to full-screen top-level views.
//
// These lock the structural pieces JS and CSS depend on so a refactor can't
// silently break the rail nav, the view-switching CSS, or the cron-view
// decoupling from selectedKey. They complement (not replace) the existing
// CSP wiring test (dashboard_csp_test.go) which still pins #btn-cron.
package server

import (
	"strings"
	"testing"
)

// TestDashboardHTML_RailStructure pins the rail's top/bottom groups and the
// new nav buttons + connection indicator. The rail is the single nav surface
// after this redesign, so these ids/classes are load-bearing for both the
// addEventListener wiring and the mobile bottom-tab-bar CSS.
func TestDashboardHTML_RailStructure(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)

	wants := []string{
		`class="ab-top"`,                   // top nav group
		`class="ab-bottom"`,                // bottom group (settings + conn)
		`id="abnav-cron" data-view="cron"`, // 自动化 nav button
		`id="abnav-settings" data-view="settings"`,
		`id="ab-conn-status"`, // connection indicator (also settings entry)
		`id="ab-conn-dot"`,
		`id="ab-conn-label"`,
		`id="abnav-cron-badge"`, // rail attention badge mirror
	}
	for _, w := range wants {
		if !strings.Contains(html, w) {
			t.Errorf("dashboard.html missing rail element: %q", w)
		}
	}
	// The bottom group must be pushed to the rail foot.
	if !strings.Contains(html, ".ab-bottom{margin-top:auto") {
		t.Error("CSS: .ab-bottom must use margin-top:auto to sit at the rail foot")
	}
	// On mobile the two groups collapse so all 4 buttons become bottom-tab-bar
	// flex children.
	if !strings.Contains(html, ".ab-top,.ab-bottom{display:contents}") {
		t.Error("CSS: .ab-top/.ab-bottom must collapse via display:contents on mobile")
	}
}

// TestDashboardHTML_TopLevelViewContainers pins the resident #cron-main and
// #settings-main containers + the mutually-exclusive view-switching CSS that
// shows exactly one view at a time.
func TestDashboardHTML_TopLevelViewContainers(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)

	// Containers exist and are hidden by default (shown only under their view
	// class).
	for _, w := range []string{
		`class="cron-main" id="cron-main"`,
		`class="settings-main" id="settings-main"`,
	} {
		if !strings.Contains(html, w) {
			t.Errorf("dashboard.html missing view container: %q", w)
		}
	}
	if !strings.Contains(html, `id="cron-main" aria-label="定时任务" hidden`) {
		t.Error("#cron-main must be hidden by default")
	}
	if !strings.Contains(html, `id="settings-main" aria-label="设置" hidden`) {
		t.Error("#settings-main must be hidden by default")
	}

	// View-switch CSS: each non-chat view hides the chat panels and shows its
	// own. The default-hidden rule must cover the new containers too.
	wantCSS := []string{
		".asset-sidebar,.asset-main,.cron-main,.settings-main{display:none}",
		"body.nz-view-cron .sidebar,body.nz-view-cron .resizer,body.nz-view-cron .main{display:none!important}",
		"body.nz-view-cron .cron-main{display:flex",
		"body.nz-view-settings .sidebar,body.nz-view-settings .resizer,body.nz-view-settings .main{display:none!important}",
		"body.nz-view-settings .settings-main{display:flex",
	}
	for _, w := range wantCSS {
		if !strings.Contains(html, w) {
			t.Errorf("dashboard.html missing view-switch CSS: %q", w)
		}
	}
}

// TestDashboardJS_ActivityViewRouter pins the JS view-router contract:
//   - top-level setActivityView (callable from openCronPanel/selectSession)
//   - cron rendering gated on activeView (NOT selectedKey) and targeting
//     #cron-main (NOT #main)
//   - renderSettingsView / renderRailConnStatus exist
func TestDashboardJS_ActivityViewRouter(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	wants := []string{
		"function setActivityView(",      // top-level router
		"let activeView = 'chat'",        // root view state
		"function renderSettingsView(",   // settings view renderer
		"function renderRailConnStatus(", // bottom-rail conn indicator
	}
	for _, w := range wants {
		if !strings.Contains(js, w) {
			t.Errorf("dashboard.js missing: %q", w)
		}
	}
	// renderCronPanel must gate on the active view and paint into #cron-main —
	// the decoupling from selectedKey is the core of promoting cron to a
	// top-level view. If either reverts, async cron repaints could clobber the
	// chat DOM again.
	if !strings.Contains(js, "if (activeView !== 'cron') return;") {
		t.Error("renderCronPanel must gate on activeView !== 'cron' (not selectedKey)")
	}
	if !strings.Contains(js, "const main = document.getElementById('cron-main');") {
		t.Error("renderCronPanel must render into #cron-main (not #main)")
	}
}

// TestDashboardJS_CronAttentionSingleFilter pins R20260606-CODE-1: within
// fetchCronJobs, the attention count (paused/errored/missed jobs) must be
// computed exactly once and shared by both cronBadge and railBadge.
func TestDashboardJS_CronAttentionSingleFilter(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Extract the body of fetchCronJobs by finding its function boundary.
	funcStart := strings.Index(js, "async function fetchCronJobs()")
	if funcStart == -1 {
		t.Fatal("fetchCronJobs not found in dashboard.js")
	}
	// Find the matching closing brace by scanning from the opening '{'.
	openBrace := strings.Index(js[funcStart:], "{")
	if openBrace == -1 {
		t.Fatal("fetchCronJobs opening brace not found")
	}
	body := js[funcStart+openBrace:]
	depth, end := 0, -1
	for i, ch := range body {
		if ch == '{' {
			depth++
		} else if ch == '}' {
			depth--
			if depth == 0 {
				end = i
				break
			}
		}
	}
	if end == -1 {
		t.Fatal("fetchCronJobs closing brace not found")
	}
	fnBody := body[:end+1]

	const filterExpr = `cronJobs.filter(j => j.paused || j.last_error || j.missed).length`
	// Within fetchCronJobs the expression must appear exactly once — a second
	// occurrence would recompute on potentially stale data and is the defect we fixed.
	first := strings.Index(fnBody, filterExpr)
	if first == -1 {
		t.Fatalf("attention filter expression not found in fetchCronJobs body")
	}
	if second := strings.Index(fnBody[first+len(filterExpr):], filterExpr); second != -1 {
		t.Error("attention filter computed twice within fetchCronJobs — must be hoisted to a single const shared by cronBadge and railBadge")
	}
	// Both badges must reference the shared `attention` variable, not a new filter.
	if !strings.Contains(fnBody, "railBadge.hidden = attention === 0") {
		t.Error("railBadge must reference shared `attention` variable, not recompute")
	}
}

// TestDashboardJS_SetActivityViewNoOpGuard pins R20260606-CODE-2: clicking an
// already-active rail button must be a no-op (no WS teardown / re-fetch).
// The guard must appear after the validity normalisation so an invalid view
// is corrected to 'chat' before the no-op check runs.
func TestDashboardJS_SetActivityViewNoOpGuard(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// The validity gate must come before the no-op guard.
	validityGate := `if (ACTIVITY_VIEWS.indexOf(view) === -1) view = 'chat';`
	noOpGuard := `if (view === activeView) return;`

	vIdx := strings.Index(js, validityGate)
	nIdx := strings.Index(js, noOpGuard)
	if vIdx == -1 {
		t.Fatal("validity gate not found in setActivityView")
	}
	if nIdx == -1 {
		t.Fatal("no-op guard 'if (view === activeView) return;' not found in setActivityView")
	}
	if nIdx < vIdx {
		t.Error("no-op guard appears before validity normalisation; must come after so invalid→'chat' is corrected first")
	}
}

// TestDashboardJS_ValidDotClassesIncludesUnreachable pins R20260606-CODE-3:
// 'unreachable' must be in VALID_DOT_CLASSES so ab-conn-dot and ns-dot
// elements receive the correct CSS class (whose rules exist in dashboard.html)
// instead of falling back to 'offline'.
func TestDashboardJS_ValidDotClassesIncludesUnreachable(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	if !strings.Contains(js, `unreachable: 'unreachable'`) {
		t.Error("VALID_DOT_CLASSES must include unreachable: 'unreachable' so the CSS rule is not a dead rule")
	}
}

// TestDashboardHTML_RailA11yLabelsLocalized keeps the new rail buttons'
// aria-labels in Chinese, consistent with the R149 localization contract for
// top-nav controls.
func TestDashboardHTML_RailA11yLabelsLocalized(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)
	wants := []string{
		`id="abnav-cron" data-view="cron" title="定时任务" aria-label="自动化视图"`,
		`id="abnav-settings" data-view="settings" title="设置" aria-label="设置视图"`,
		`id="ab-conn-status" type="button" title="连接状态"`,
	}
	for _, w := range wants {
		if !strings.Contains(html, w) {
			t.Errorf("dashboard.html missing localized rail label: %q", w)
		}
	}
}
