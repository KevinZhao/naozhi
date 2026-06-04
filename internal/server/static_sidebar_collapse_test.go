package server

import (
	"strings"
	"testing"
)

// TestDashboardSidebarCollapseContract pins the PC-only "fully-collapse the
// left sidebar" affordance:
//
//   - HTML: a single mid-line handle (#btn-sidebar-toggle) lives inside the
//     resizer strip and serves both directions (collapse + restore). Notion /
//     Cursor pattern: lower noise than a separate header button + floating
//     restore handle, and the toggle's position stays continuous across the
//     two states.
//   - CSS: body.sidebar-collapsed hides .sidebar at min-width:769px while
//     leaving the resizer strip in place so its handle can drive restore.
//     The handle's chevron flips via transform:rotate(180deg) so the same
//     SVG serves both directions. Mobile (≤768px) keeps the existing drawer
//     contract untouched, so the collapse class is a no-op there.
//   - JS: toggleSidebarCollapsed() flips body.sidebar-collapsed and persists
//     state via lsSet under the 'sidebar_collapsed' key. The `[` keyboard
//     shortcut triggers the same path. Mobile viewports short-circuit so the
//     PC collapse never collides with mobile-list-view / mobile-chat-view.
//     The resizer's mousedown / dblclick handlers must skip when the click
//     originates inside the handle, otherwise the click would start a drag.
//
// Locking these wires together prevents accidental drift — e.g. removing the
// CSS gating without removing the toggle button, or vice-versa.
func TestDashboardSidebarCollapseContract(t *testing.T) {
	t.Parallel()

	htmlData, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(htmlData)

	jsData, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(jsData)

	// HTML: the unified mid-line toggle on the resizer.
	if !strings.Contains(html, `id="btn-sidebar-toggle"`) {
		t.Error("dashboard.html: missing #btn-sidebar-toggle on the resizer")
	}
	// The handler is bound via addEventListener in dashboard.js (#922 / #479
	// inline-handler migration) rather than an inline onclick=.
	if !strings.Contains(js, `bindClick('btn-sidebar-toggle', function () { toggleSidebarCollapsed(); });`) {
		t.Error("dashboard.js: btn-sidebar-toggle click must bind toggleSidebarCollapsed() via addEventListener")
	}
	if !strings.Contains(html, `class="resizer-handle"`) {
		t.Error("dashboard.html: toggle button must use .resizer-handle for CSS gating")
	}

	// HTML: aria-controls must point at a real element id.
	if !strings.Contains(html, `<nav class="sidebar" id="sidebar"`) {
		t.Error("dashboard.html: sidebar nav must carry id=\"sidebar\" for aria-controls to be valid")
	}
	if !strings.Contains(html, `aria-controls="sidebar"`) {
		t.Error("dashboard.html: toggle button must declare aria-controls=\"sidebar\"")
	}

	// HTML: handle lives INSIDE the resizer so its position is continuous
	// across collapse states (no jump from sidebar header to a fixed overlay).
	resizerOpen := strings.Index(html, `class="resizer" id="resizer"`)
	handleIdx := strings.Index(html, `id="btn-sidebar-toggle"`)
	if resizerOpen < 0 || handleIdx < 0 {
		t.Fatal("dashboard.html: resizer / toggle markers missing — test obsolete")
	}
	if handleIdx < resizerOpen {
		t.Error("dashboard.html: #btn-sidebar-toggle must live INSIDE the .resizer element")
	}

	// HTML: the previous round's separate header button + floating restore
	// handle were removed in favor of the single mid-line handle. Pin their
	// absence so they don't get re-introduced.
	for _, gone := range []string{
		`id="btn-sidebar-collapse"`,
		`id="btn-sidebar-show"`,
		`class="sidebar-show-handle"`,
	} {
		if strings.Contains(html, gone) {
			t.Errorf("dashboard.html: %q is the old contract — should be removed in favor of the unified .resizer-handle", gone)
		}
	}

	// CSS: collapse hides only .sidebar (resizer stays so handle can restore).
	for _, want := range []string{
		`body.sidebar-collapsed .sidebar{display:none}`,
		// Chevron flips via rotate so one SVG serves both directions.
		`body.sidebar-collapsed .resizer-handle svg{transform:rotate(180deg)}`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard.html CSS missing collapse rule: %q", want)
		}
	}
	// CSS gating must be inside a min-width:769px media block so mobile keeps
	// its drawer contract.
	if !strings.Contains(html, "@media(min-width:769px)") {
		t.Error("dashboard.html: collapse rules must be gated by @media(min-width:769px)")
	}

	// JS: toggle helper + persistence key + keyboard shortcut + mobile guard
	// + IME-composition guard + viewport-boundary listener + focus relocation.
	for _, want := range []string{
		`function toggleSidebarCollapsed()`,
		`function applySidebarCollapsed(collapsed, moveFocus)`,
		`'sidebar-collapsed'`,
		`LS_SIDEBAR_COLLAPSED = 'sidebar_collapsed'`,
		`lsSet(LS_SIDEBAR_COLLAPSED`,
		`lsGet(LS_SIDEBAR_COLLAPSED`,
		// Single toggle button id targeted by applySidebarCollapsed.
		`getElementById('btn-sidebar-toggle')`,
		// Keyboard shortcut: `[` triggers toggle outside inputs.
		`if (e.key !== '[')`,
		// CJK IME composition: don't fire while a composition is active.
		`if (e.isComposing) return;`,
		// Mobile guard: matchMedia(max-width: 768px) short-circuits the toggle.
		`(max-width: 768px)`,
		// Focus relocation: user-driven toggle hands focus to the now-visible
		// button so keyboard nav doesn't fall back to <body>.
		`btn.focus({preventScroll: true})`,
		// Viewport-boundary listener: re-applies preference when crossing
		// the mobile breakpoint (DevTools / rotation / resize).
		`mql.addEventListener('change'`,
		// Resizer drag must skip clicks landing on the handle, otherwise
		// the click would start a drag instead of toggling collapse.
		`closest('.resizer-handle')`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("dashboard.js missing collapse-related fragment: %q", want)
		}
	}
}
