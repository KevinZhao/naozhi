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

// TestCronLayout_PreJSFallbackStandsDown pins the one piece of the cron layout
// tier the behaviour spec (test/e2e/cj_row_narrow.test.js) cannot reach: the
// @media fallback for the paint before setupCronLayoutObserver runs must be
// guarded with :not([data-cron-layout]), or it keeps overriding the tier the
// observer sets once JS is up.
func TestCronLayout_PreJSFallbackStandsDown(t *testing.T) {
	t.Parallel()
	html := readDashboardHTMLAndCSS(t)
	if !strings.Contains(html, ".cron-detail-body:not([data-cron-layout]).has-drawer > .cron-list-pane{display:none}") {
		t.Error("cron.css: the pre-JS @media fallback must guard with :not([data-cron-layout]) so it stands down once the observer sets the tier")
	}
}
