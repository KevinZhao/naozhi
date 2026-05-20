package server

import (
	"strings"
	"testing"
)

// TestDashboardSidebarCollapseContract pins the PC-only "fully-collapse the
// left sidebar" affordance:
//
//   - HTML: a header toggle (#btn-sidebar-collapse) inside the sidebar header,
//     plus a fixed-position restore handle (#btn-sidebar-show) outside the
//     container so it stays visible after the sidebar is hidden.
//   - CSS: body.sidebar-collapsed hides .sidebar + .resizer at min-width:769px,
//     and reveals #btn-sidebar-show. Mobile (≤768px) keeps the existing drawer
//     contract untouched, so the collapse class is a no-op there.
//   - JS: toggleSidebarCollapsed() flips body.sidebar-collapsed and persists
//     state via lsSet under the 'sidebar_collapsed' key. The `[` keyboard
//     shortcut triggers the same path. Mobile viewports short-circuit so the
//     PC collapse never collides with mobile-list-view / mobile-chat-view.
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

	// HTML: the in-sidebar collapse trigger.
	if !strings.Contains(html, `id="btn-sidebar-collapse"`) {
		t.Error("dashboard.html: missing #btn-sidebar-collapse trigger inside sidebar header")
	}
	if !strings.Contains(html, `onclick="toggleSidebarCollapsed()"`) {
		t.Error("dashboard.html: collapse buttons must call toggleSidebarCollapsed()")
	}

	// HTML: aria-controls must point at a real element id. Both triggers
	// reference the sidebar; the sidebar must carry id="sidebar".
	if !strings.Contains(html, `<nav class="sidebar" id="sidebar"`) {
		t.Error("dashboard.html: sidebar nav must carry id=\"sidebar\" for aria-controls to be valid")
	}
	if !strings.Contains(html, `aria-controls="sidebar"`) {
		t.Error("dashboard.html: collapse triggers must declare aria-controls=\"sidebar\"")
	}

	// HTML: the floating restore handle, kept outside .container so its
	// position:fixed isn't affected by mobile drawer transforms on .sidebar.
	if !strings.Contains(html, `id="btn-sidebar-show"`) {
		t.Error("dashboard.html: missing #btn-sidebar-show restore handle")
	}
	if !strings.Contains(html, `class="sidebar-show-handle"`) {
		t.Error("dashboard.html: restore handle must use .sidebar-show-handle for CSS gating")
	}
	// Restore handle must NOT live inside .container — it's a fixed overlay
	// and being inside the flex container made the sidebar's transforms
	// translate it offscreen on the mobile breakpoint during early prototypes.
	containerOpen := strings.Index(html, `<div class="container">`)
	containerClose := strings.Index(html, `</div>`+"\n"+`<!-- Sidebar restore handle`)
	handleIdx := strings.Index(html, `id="btn-sidebar-show"`)
	if containerOpen < 0 || handleIdx < 0 {
		t.Fatal("dashboard.html: container open / restore-handle markers missing — test obsolete")
	}
	if containerClose < 0 || handleIdx < containerClose {
		t.Error("dashboard.html: #btn-sidebar-show must live OUTSIDE .container (after </div>)")
	}

	// CSS: gating + restore-handle reveal + main-header padding all keyed
	// off body.sidebar-collapsed. The padding rule is the fix for the
	// floating handle (32×32 @ left:10px) overlapping the chat title;
	// dropping it puts the handle on top of h2 text on cold-load.
	for _, want := range []string{
		// Combined rule: .sidebar AND .resizer share a single display:none.
		`body.sidebar-collapsed .sidebar,
  body.sidebar-collapsed .resizer{display:none}`,
		`body.sidebar-collapsed .sidebar-show-handle{display:inline-flex}`,
		`body.sidebar-collapsed .main-header{padding-left:54px}`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard.html CSS missing collapse rule: %q", want)
		}
	}
	// CSS gating must be inside a min-width:769px media block so mobile keeps
	// its drawer contract. We don't pin the exact media-query syntax — just
	// require the collapse rules and an min-width:769px gate to coexist.
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
		// Keyboard shortcut: `[` triggers toggle outside inputs.
		`if (e.key !== '[')`,
		// CJK IME composition: don't fire while a composition is active.
		`if (e.isComposing) return;`,
		// Mobile guard: matchMedia(max-width: 768px) short-circuits the toggle.
		`(max-width: 768px)`,
		// Focus relocation: user-driven toggle hands focus to the now-visible
		// button so keyboard nav doesn't fall back to <body>.
		`next.focus({preventScroll: true})`,
		// Viewport-boundary listener: re-applies preference when crossing
		// the mobile breakpoint (DevTools / rotation / resize).
		`mql.addEventListener('change'`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("dashboard.js missing collapse-related fragment: %q", want)
		}
	}

	// JS: dead code that the previous round shipped — the title-rewrite
	// branch on btnHide could never be visible because btnHide is
	// display:none'd in the same paint as collapsed=true. Pin its absence
	// so it doesn't get re-introduced by a future "fix".
	if strings.Contains(js, "btnHide.title = collapsed") {
		t.Error("dashboard.js: btnHide.title rewrite is dead code (button is display:none in the collapsed paint); remove it")
	}
}
