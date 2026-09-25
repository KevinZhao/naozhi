package server

import (
	"strings"
	"testing"
)

// TestDashboardHTML_CronPanelConsolidationStyles pins the CSS shell that the
// drawer needs. If any of these classes are deleted by a future "dead CSS
// cleanup" pass, the drawer will render but look broken.
func TestDashboardHTML_CronPanelConsolidationStyles(t *testing.T) {
	t.Parallel()
	html := readDashboardHTMLAndCSS(t)

	// Two-column shell.
	for _, sel := range []string{
		".cron-detail-body",
		".cron-list-pane",
		".cron-detail-pane",
		".cron-detail-pane.is-open",
		".cron-detail-body.has-drawer",
	} {
		if !strings.Contains(html, sel+"{") && !strings.Contains(html, sel+" ") && !strings.Contains(html, sel+"."+"") && !strings.Contains(html, sel+">") {
			t.Errorf("dashboard.html: missing %s rule — drawer two-column layout broken", sel)
		}
	}

	// Drawer sections — at least one rule per class.
	for _, sel := range []string{
		".cron-drawer-header",
		".cron-drawer-summary",
		".cron-drawer-actions",
		".cron-drawer-current",
		".cron-drawer-history",
		".cron-drawer-empty",
	} {
		if !strings.Contains(html, sel) {
			t.Errorf("dashboard.html: missing %s — drawer section CSS broken", sel)
		}
	}

	// Active-row highlight.
	if !strings.Contains(html, ".cj-row.is-active") {
		t.Error("dashboard.html: missing .cj-row.is-active — list selection highlight broken")
	}

	// Retired CSS rules must be gone (paired with
	// TestDashboardJS_CronSessionsHiddenByDefault). Match the rule body
	// (`{`) rather than the bare selector so the explanatory comments in
	// dashboard.html that name retired classes don't trip the test.
	for _, ruleStart := range []string{
		".session-card.sc-cron-card{",
		".cron-detail .cd-field{",
		".cron-detail .cd-result{",
		".cron-card .cc-actions{",
		".cron-card .cc-btn{",
	} {
		if strings.Contains(html, ruleStart) {
			t.Errorf("dashboard.html: retired %s… rule still present — cron-panel-consolidation §4.7 cleanup incomplete", ruleStart)
		}
	}
}

// TestDashboardJS_R2_R1_LayoutObserver pins the Round 2 review R-1 fix:
// responsive layout for the cron panel keys off the *main column width*
// (ResizeObserver) instead of viewport @media. Three tier breakpoints
// + single-column collapse must be wired so a 1080p user with a wide
// sidebar doesn't get a 240px-wide drawer.
func TestDashboardJS_R2_R1_LayoutObserver(t *testing.T) {
	t.Parallel()
	jsData, err := cronViewJS.ReadFile("static/cron_view.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(jsData)
	htmlData := []byte(readDashboardHTMLAndCSS(t))
	html := string(htmlData)

	// 1. setupCronLayoutObserver must exist and fire from renderCronPanel.
	if !strings.Contains(js, "function setupCronLayoutObserver()") {
		t.Error("dashboard.js: setupCronLayoutObserver() helper required for R-1 ResizeObserver wiring")
	}
	// renderCronPanel calls it after shell mount.
	rpIdx := strings.Index(js, "function renderCronPanel()")
	if rpIdx < 0 {
		t.Fatal("dashboard.js: renderCronPanel not found")
	}
	// renderCronPanel emits a long inline HTML string + paint hooks; the
	// setupCronLayoutObserver call sits near the bottom right before the
	// next top-level function. Bound the search by that next declaration
	// rather than a fragile byte count.
	nextFn := strings.Index(js[rpIdx+24:], "\nfunction ")
	rpEnd := len(js)
	if nextFn >= 0 {
		rpEnd = rpIdx + 24 + nextFn
	}
	if !strings.Contains(js[rpIdx:rpEnd], "setupCronLayoutObserver()") {
		t.Error("renderCronPanel: must invoke setupCronLayoutObserver() after shell mount so the data-cron-layout attribute is initialised")
	}

	// 2. Three breakpoints must appear in the JS. cron-dashboard-redesign
	// P0 follow-up retuned the thresholds (gauging off list-pane width
	// instead of body width): wide ≥600, medium ≥420, narrow ≥300.
	for _, threshold := range []string{"600", "420", "300"} {
		if !strings.Contains(js, "lpW >= "+threshold) {
			t.Errorf("setupCronLayoutObserver: must compare against %s threshold (RFC §2 four-tier matrix)", threshold)
		}
	}

	// 3. CSS must key off [data-cron-layout="…"] on .cron-detail-body.
	for _, mode := range []string{"wide", "medium", "narrow", "single"} {
		needle := ".cron-detail-body[data-cron-layout=\"" + mode + "\"]"
		if !strings.Contains(html, needle) {
			t.Errorf("dashboard.html: missing CSS rule %s — RFC §2 layout tier", needle)
		}
	}

	// 4. Pre-JS @media fallback must use :not([data-cron-layout]) so it
	//    deactivates as soon as JS sets the attribute.
	if !strings.Contains(html, ":not([data-cron-layout])") {
		t.Error("dashboard.html: pre-JS @media fallback must guard with :not([data-cron-layout]) so it deactivates once JS runs")
	}
}
