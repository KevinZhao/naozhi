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

// TestDashboardJS_SidebarAutoRestoreWired pins the symmetric half of the
// contract: closing the last right-side drawer re-expands a sidebar that the
// open path auto-collapsed. Both close paths (closeFilePreview for the
// preview/code-block drawer, scratch hideDrawer for 追问) must route through
// restoreSidebarAfterDrawer, and the restore must stay transient (no
// localStorage write) and conditional (never undo a user-chosen collapse).
func TestDashboardJS_SidebarAutoRestoreWired(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read embedded dashboard.js: %v", err)
	}
	js := string(data)

	if !strings.Contains(js, "function restoreSidebarAfterDrawer(") {
		t.Fatal("restoreSidebarAfterDrawer helper missing from dashboard.js — sidebar auto-restore on drawer close is unwired")
	}

	// Exactly the two drawer close paths must invoke the helper
	// (closeFilePreview + scratch hideDrawer). previewCodeBlock shares
	// closeFilePreview, so two call sites cover all three open entry points.
	const call = "restoreSidebarAfterDrawer();"
	if got := strings.Count(js, call); got != 2 {
		t.Fatalf("restoreSidebarAfterDrawer() call-site count = %d, want 2 (closeFilePreview / scratch hideDrawer)", got)
	}

	body := jsFuncBody(t, js, "restoreSidebarAfterDrawer")

	// Transient, like the collapse: must not write the persistence key.
	if strings.Contains(body, "LS_SIDEBAR_COLLAPSED") || strings.Contains(body, "lsSet(") {
		t.Errorf("restoreSidebarAfterDrawer must not persist (the user's saved preference was never touched by the auto-collapse); body=%q", body)
	}

	// Must only undo a drawer-driven collapse — a user-chosen collapse
	// (persisted preference or manual toggle while the drawer was open) stays.
	if !strings.Contains(body, "_sidebarAutoCollapsed") {
		t.Error("restoreSidebarAfterDrawer must gate on _sidebarAutoCollapsed so it never expands a sidebar the user collapsed themselves")
	}

	// Preview and 追问 can be docked simultaneously; only the LAST close may
	// restore. The helper must consult the shared drawer-open guard (exported
	// by the split-view block as nzAnyDrawerOpen) rather than its own copy.
	if !strings.Contains(body, "nzAnyDrawerOpen") {
		t.Error("restoreSidebarAfterDrawer must bail via nzAnyDrawerOpen while a drawer is still open — only the last close restores")
	}
	if !strings.Contains(js, "window.nzAnyDrawerOpen = anyDrawerOpen") {
		t.Error("split-view block must export anyDrawerOpen as window.nzAnyDrawerOpen for the sidebar-restore guard")
	}

	// The collapse side must arm the flag, and a manual toggle must disarm it
	// so the user's explicit choice wins over the pending auto-restore. The
	// viewport-boundary handler re-derives sidebar state wholesale, so it too
	// must disarm a pending restore.
	if !strings.Contains(jsFuncBody(t, js, "collapseSidebarForDrawer"), "_sidebarAutoCollapsed = true") {
		t.Error("collapseSidebarForDrawer must set _sidebarAutoCollapsed = true to arm the restore")
	}
	if !strings.Contains(jsFuncBody(t, js, "toggleSidebarCollapsed"), "_sidebarAutoCollapsed = false") {
		t.Error("toggleSidebarCollapsed must clear _sidebarAutoCollapsed — a manual toggle takes ownership of the sidebar state")
	}
	if got := strings.Count(js, "_sidebarAutoCollapsed = false"); got < 3 {
		t.Errorf("_sidebarAutoCollapsed disarm count = %d, want ≥3 (toggle / restore / onMqlChange viewport-boundary handler)", got)
	}
}

// jsFuncBody extracts the source of `function <name>(...)` from js up to the
// first column-0 closing brace. Fails the test cleanly (instead of panicking
// on a negative slice index) when the function is missing.
func jsFuncBody(t *testing.T, js, name string) string {
	t.Helper()
	decl := "function " + name + "("
	start := strings.Index(js, decl)
	if start < 0 {
		t.Fatalf("could not find %q in dashboard.js", decl)
	}
	end := strings.Index(js[start:], "\n}")
	if end < 0 {
		t.Fatalf("could not bound %s body", name)
	}
	return js[start : start+end]
}
