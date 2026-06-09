package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_SidebarAutoCollapseWired pins the UX contract that opening a
// right-side drawer (追问 scratch / file preview / code-block preview) auto-
// collapses the sidebar to free horizontal space. We assert on the embedded
// dashboard.js source so a future refactor that drops one of the three call
// sites — or the shared helper itself — fails CI instead of silently
// regressing the behavior in the browser.
func TestDashboardJS_SidebarAutoCollapseWired(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read embedded dashboard.js: %v", err)
	}
	js := string(data)

	// The shared helper must exist; all three drawers route through it so the
	// mobile-skip + idempotency guard live in one place.
	if !strings.Contains(js, "function collapseSidebarForDrawer(") {
		t.Fatal("collapseSidebarForDrawer helper missing from dashboard.js — sidebar auto-collapse on drawer open is unwired")
	}

	// Exactly the three drawer entry points must invoke the helper. We count
	// call sites (not the declaration) to catch both a dropped wiring and an
	// accidental stray call that would collapse the sidebar in the wrong flow.
	const call = "collapseSidebarForDrawer();"
	if got := strings.Count(js, call); got != 3 {
		t.Fatalf("collapseSidebarForDrawer() call-site count = %d, want 3 (追问 / file preview / code-block preview)", got)
	}

	// The helper must NOT persist the collapse — a transient, context-driven
	// collapse should never overwrite the user's own toggle preference. Verify
	// the helper body does not touch the persistence key.
	start := strings.Index(js, "function collapseSidebarForDrawer(")
	end := strings.Index(js[start:], "\n}")
	if end < 0 {
		t.Fatal("could not bound collapseSidebarForDrawer body")
	}
	body := js[start : start+end]
	if strings.Contains(body, "LS_SIDEBAR_COLLAPSED") || strings.Contains(body, "lsSet(") {
		t.Errorf("collapseSidebarForDrawer must not persist the collapse (would clobber the user's saved sidebar preference); body=%q", body)
	}
}
