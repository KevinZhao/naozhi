package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_ProjectDisplayName pins the R110-P2 / #448 dashboard
// wiring for project display_name + emoji:
//
//  1. projectDisplayLabel(p) and projectDisplayPrefix(p) helpers exist
//     so callers route through one canonical place — a future change to
//     "display_name + sub-team" semantics only touches these helpers.
//  2. buildProjectRow (palette) uses the helpers so the cmd-palette
//     reflects the operator's chosen label.
//  3. sectionHeaderHtml (sidebar) uses the helpers and keeps p.name in
//     the title attribute so screen-reader / hover users can still see
//     the directory-derived name when a display_name is configured.
//  4. The fallback path: when display_name is empty, both helpers must
//     return the legacy `p.name`. The pure-JS branches `(cfg.display_name
//     || ”).trim()` followed by `if (dn) return dn` make this provable
//     without a JS runtime.
func TestDashboardJS_ProjectDisplayName(t *testing.T) {
	t.Parallel()

	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// 1. Helpers must exist and read from p.config.display_name /
	// p.config.emoji — that is the JSON shape of the /api/projects
	// response (project_api.go projectsListEntry.Config field).
	if !strings.Contains(js, "function projectDisplayLabel(p) {") {
		t.Error("dashboard.js missing projectDisplayLabel(p) — R110-P2 / #448 expects a single helper")
	}
	if !strings.Contains(js, "function projectDisplayPrefix(p) {") {
		t.Error("dashboard.js missing projectDisplayPrefix(p) — emoji prefix helper")
	}
	for _, sym := range []string{"cfg.display_name", "cfg.emoji"} {
		if !strings.Contains(js, sym) {
			t.Errorf("dashboard.js display-name helpers must read %s (mirrors the /api/projects JSON wire shape)", sym)
		}
	}

	// 2. Palette row must call into the helpers so the cmd-palette
	// reflects the configured emoji + display name.
	rowIdx := strings.Index(js, "function buildProjectRow(s, idx) {")
	if rowIdx < 0 {
		t.Fatal("dashboard.js missing buildProjectRow")
	}
	rowEnd := rowIdx + 2000
	if rowEnd > len(js) {
		rowEnd = len(js)
	}
	rowBody := js[rowIdx:rowEnd]
	for _, want := range []string{"projectDisplayPrefix(p)", "projectDisplayLabel(p)"} {
		if !strings.Contains(rowBody, want) {
			t.Errorf("buildProjectRow must call %s — palette must reflect display_name / emoji per #448", want)
		}
	}

	// 3. Sidebar header (sectionHeaderHtml) must call into the helpers
	// AND keep p.name reachable in either the aria-label or the title
	// so screen-readers / hover tooltips disambiguate.
	hdrIdx := strings.Index(js, "function sectionHeaderHtml(p) {")
	if hdrIdx < 0 {
		t.Fatal("dashboard.js missing sectionHeaderHtml")
	}
	// sectionHeaderHtml ends a few hundred lines later; cap at 4 KiB
	// so the search window fits the function body even after future
	// growth.
	hdrEnd := hdrIdx + 4000
	if hdrEnd > len(js) {
		hdrEnd = len(js)
	}
	hdrBody := js[hdrIdx:hdrEnd]
	for _, want := range []string{"projectDisplayPrefix(p)", "projectDisplayLabel(p)"} {
		if !strings.Contains(hdrBody, want) {
			t.Errorf("sectionHeaderHtml must call %s for display_name / emoji rendering", want)
		}
	}
	// p.name must remain visible in the header path — either in aria-
	// label or title — so existing keyboard / screen-reader users can
	// still see the directory name when display_name diverges.
	if !strings.Contains(hdrBody, "p.name") {
		t.Error("sectionHeaderHtml must keep a p.name reference (aria-label / title) so the dirname stays discoverable when display_name overrides the visible label")
	}
}
