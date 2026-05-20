package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_CronPanelConsolidation pins the cron-panel-consolidation
// RFC's drawer invariants. It complements TestDashboardJS_CronSessionsHiddenByDefault
// (which proves sidebar / mainShell shed cron) by proving the per-job
// drawer in the 定时任务 panel is correctly wired:
//
//  1. cronDetailJobId state — module-scoped, null by default
//  2. openCronDetail(jobId) / closeCronDetail() lifecycle
//  3. cronJobCardHtml routes clicks into openCronDetail (not openCronSession,
//     not selectSession) and applies .cj-row.is-active when matched
//  4. cronDrawerHtml emits the 6 sections (header / summary / actions /
//     current / history with timeline host / empty fallback)
//  5. cronTimelineRefreshHead / cronTimelineFetchDetail / cronTimelineLoadMore
//     gate on cronDetailJobId rather than selectedKey
//  6. doCreateCronJob calls openCronDetail with the new job's id so the
//     drawer surfaces the just-created task
//  7. cronDelete clears cronDetailJobId when removing the active job so
//     the drawer doesn't latch onto a deleted phantom
func TestDashboardJS_CronPanelConsolidation(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// 1. Module-scoped cronDetailJobId state.
	if !strings.Contains(js, "let cronDetailJobId = null") {
		t.Error("dashboard.js: cronDetailJobId must be declared as module-scope `let cronDetailJobId = null` — drawer state-machine entry point")
	}

	// 2. Lifecycle helpers.
	if !strings.Contains(js, "function openCronDetail(jobId, originRow)") {
		t.Error("dashboard.js: openCronDetail(jobId, originRow) must exist as the canonical drawer-open entry point")
	}
	if !strings.Contains(js, "function closeCronDetail()") {
		t.Error("dashboard.js: closeCronDetail() must exist for ✕ button / Esc / delete-active flow")
	}

	// 3. Click routing in cronJobCardHtml.
	cardIdx := strings.Index(js, "function cronJobCardHtml(j)")
	if cardIdx < 0 {
		t.Fatal("dashboard.js: cronJobCardHtml not found")
	}
	cardEnd := cardIdx + 8000
	if cardEnd > len(js) {
		cardEnd = len(js)
	}
	cardBody := js[cardIdx:cardEnd]
	if !strings.Contains(cardBody, "onclick=\"openCronDetail(\\'' + escJs(j.id)") {
		t.Error("cronJobCardHtml: row click must invoke openCronDetail (was openCronSession before consolidation)")
	}
	if !strings.Contains(cardBody, "openCronDetail(\\'' + escJs(j.id) + '\\', this)") {
		t.Error("cronJobCardHtml: must pass `this` (the row element) into openCronDetail so closeCronDetail can restore focus (RFC §6.4)")
	}
	// is-active class wiring.
	if !strings.Contains(cardBody, "cronDetailJobId === j.id") {
		t.Error("cronJobCardHtml: must compare j.id against cronDetailJobId so the active row gains .is-active")
	}
	if !strings.Contains(cardBody, "rowClasses.push('is-active')") {
		t.Error("cronJobCardHtml: must push 'is-active' onto rowClasses when the drawer matches this job")
	}

	// 4. cronDrawerHtml structure — six sections.
	if !strings.Contains(js, "function cronDrawerHtml(j)") {
		t.Fatal("dashboard.js: cronDrawerHtml(j) must exist — owns the drawer markup")
	}
	drawerIdx := strings.Index(js, "function cronDrawerHtml(j)")
	drawerEnd := drawerIdx + 8000
	if drawerEnd > len(js) {
		drawerEnd = len(js)
	}
	drawerBody := js[drawerIdx:drawerEnd]
	for _, marker := range []string{
		"<header class=\"cron-drawer-header\">",
		"<section class=\"cron-drawer-summary\">",
		"<nav class=\"cron-drawer-actions\"",
		"<section class=\"cron-drawer-current\"",
		"<section class=\"cron-drawer-history\">",
		"id=\"cron-timeline-panel\"",
	} {
		if !strings.Contains(drawerBody, marker) {
			t.Errorf("cronDrawerHtml: missing structural marker %q — drawer 6-section contract broken", marker)
		}
	}
	// Empty-state branch is owned by renderCronDrawer (jobId set, job missing).
	rendererIdx := strings.Index(js, "function renderCronDrawer()")
	if rendererIdx < 0 {
		t.Fatal("dashboard.js: renderCronDrawer() must exist")
	}
	rendererEnd := rendererIdx + 4000
	if rendererEnd > len(js) {
		rendererEnd = len(js)
	}
	rendererBody := js[rendererIdx:rendererEnd]
	if !strings.Contains(rendererBody, "cron-drawer-empty") {
		t.Error("renderCronDrawer: missing .cron-drawer-empty placeholder for the deleted-job race")
	}

	// 5. WS / async paths use cronDetailJobId, not selectedKey.
	for _, fn := range []string{
		"async function cronTimelineRefreshHead(jobId)",
		"async function cronTimelineFetchDetail(jobId, runId)",
	} {
		idx := strings.Index(js, fn)
		if idx < 0 {
			t.Errorf("dashboard.js: %s missing", fn)
			continue
		}
		end := idx + 2500
		if end > len(js) {
			end = len(js)
		}
		body := js[idx:end]
		if strings.Contains(body, "selectedKey === 'cron:' + jobId") || strings.Contains(body, "selectedKey !== 'cron:' + jobId") {
			t.Errorf("%s: route gate must be cronDetailJobId === jobId (cron-panel-consolidation RFC §4.6) — selectedKey is null in cron panel mode", fn)
		}
		if !strings.Contains(body, "cronDetailJobId") {
			t.Errorf("%s: must consult cronDetailJobId to decide whether to repaint the drawer", fn)
		}
	}

	// 6. doCreateCronJob handoff.
	createIdx := strings.Index(js, "async function doCreateCronJob")
	if createIdx >= 0 {
		end := createIdx + 4000
		if end > len(js) {
			end = len(js)
		}
		body := js[createIdx:end]
		if !strings.Contains(body, "openCronDetail(data.id)") {
			t.Error("doCreateCronJob: must call openCronDetail(data.id) on success so the new task surfaces in the drawer")
		}
		// And must NOT do the legacy promote-to-sidebar dance.
		if strings.Contains(body, "selectSession(key, 'local')") {
			t.Error("doCreateCronJob: must not call selectSession after consolidation — cron drawer owns the post-create UX")
		}
	}

	// 7. cronDelete clears cronDetailJobId for the active row.
	delIdx := strings.Index(js, "async function cronDelete(id)")
	if delIdx >= 0 {
		end := delIdx + 2500
		if end > len(js) {
			end = len(js)
		}
		body := js[delIdx:end]
		if !strings.Contains(body, "cronDetailJobId === id") {
			t.Error("cronDelete: must check cronDetailJobId === id and clear it so the drawer closes synchronously")
		}
	}

	// 8. RFC §6.4 keyboard a11y: drawer head h2 must be a programmatic
	//    focus target (tabindex="-1"); openCronDetail must move focus
	//    there; closeCronDetail must restore focus to the originating row.
	if !strings.Contains(js, "<h2 class=\"cdh-title\" tabindex=\"-1\"") {
		t.Error("cronDrawerHtml: cdh-title <h2> must carry tabindex=\"-1\" so openCronDetail can programmatically focus it (RFC §6.4)")
	}
	openIdx := strings.Index(js, "function openCronDetail(jobId, originRow)")
	if openIdx < 0 {
		t.Error("openCronDetail: signature must accept (jobId, originRow) so the click handler can pass `this` for focus restoration")
	} else {
		end := openIdx + 3000
		if end > len(js) {
			end = len(js)
		}
		body := js[openIdx:end]
		if !strings.Contains(body, "_cronDrawerLastActiveRow") {
			t.Error("openCronDetail: must record the originating row in _cronDrawerLastActiveRow")
		}
		if !strings.Contains(body, ".cdh-title") || !strings.Contains(body, "focus") {
			t.Error("openCronDetail: must focus the drawer header .cdh-title h2 after the panel paints (RFC §6.4)")
		}
		// And must NOT have the redundant explicit renderCronPanel call we
		// removed in PR review — openCronPanel already triggers it.
		if renderCount := strings.Count(body, "renderCronPanel()"); renderCount > 0 {
			t.Errorf("openCronDetail: must not call renderCronPanel() directly (redundant — openCronPanel already does); found %d call(s)", renderCount)
		}
	}
	closeIdx := strings.Index(js, "function closeCronDetail()")
	if closeIdx >= 0 {
		end := closeIdx + 2000
		if end > len(js) {
			end = len(js)
		}
		body := js[closeIdx:end]
		if !strings.Contains(body, "_cronDrawerLastActiveRow") {
			t.Error("closeCronDetail: must consult _cronDrawerLastActiveRow to restore focus to the originating row (RFC §6.4)")
		}
	}

	// 9. Global Esc handler must route to closeCronDetail when the drawer
	//    is open. Without this hook the drawer ✕ button's title="关闭 (Esc)"
	//    is a lie. Scope the search to a window covering the global keydown
	//    listener that handles popovers.
	if !strings.Contains(js, "if (cronDetailJobId !== null) { closeCronDetail();") {
		t.Error("Global Esc handler must call closeCronDetail() when cronDetailJobId !== null (RFC §6.4)")
	}

	// 10. renderCronDrawer's "task missing" branch must guard against
	//     infinite recursion via _cronDrawerFetchedFor — otherwise a system
	//     in which every cron has been deleted will loop forever when the
	//     operator deep-links to a deleted job.
	rendererIdx2 := strings.Index(js, "function renderCronDrawer()")
	if rendererIdx2 >= 0 {
		end := rendererIdx2 + 4000
		if end > len(js) {
			end = len(js)
		}
		body := js[rendererIdx2:end]
		if !strings.Contains(body, "_cronDrawerFetchedFor.has(cronDetailJobId)") {
			t.Error("renderCronDrawer: missing-job branch must consult _cronDrawerFetchedFor before fetchCronJobs to prevent infinite recursion when the system has zero cron jobs")
		}
	}

	// 11. RFC §9.3 / §9.4: 1Hz running-tick timer must use a targeted
	//     paint (cronRunningTickPaint) that updates only the running
	//     clocks, NOT a full renderCronPanel() rebuild — otherwise text
	//     selection / scroll / focus inside drawer timeline detail
	//     blocks gets wiped every second when any cron is running.
	if !strings.Contains(js, "function cronRunningTickPaint()") {
		t.Error("dashboard.js: cronRunningTickPaint() must exist as the lightweight 1Hz update path (RFC §9.3 \"重绘抽屉头计时器\")")
	}
	tickIdx := strings.Index(js, "function ensureCronRunningTick()")
	if tickIdx >= 0 {
		end := tickIdx + 2000
		if end > len(js) {
			end = len(js)
		}
		body := js[tickIdx:end]
		if !strings.Contains(body, "cronRunningTickPaint()") {
			t.Error("ensureCronRunningTick: timer body must call cronRunningTickPaint() (not renderCronPanel) — full rebuild every second wipes user text selection / scroll inside drawer timeline blocks")
		}
		if strings.Contains(body, "try { renderCronPanel()") || strings.Contains(body, "renderCronPanel(); }") {
			t.Error("ensureCronRunningTick: must NOT call renderCronPanel() inside the 1Hz tick — use cronRunningTickPaint() for surgical clock updates")
		}
	}

	// 12. RFC §9.4 perf hygiene: openCronDetail must short-circuit when
	//     called with the same jobId already on display so a second click
	//     doesn't fire another fetchCronJobs / re-steal focus to the h2.
	openIdx2 := strings.Index(js, "function openCronDetail(jobId, originRow)")
	if openIdx2 >= 0 {
		end := openIdx2 + 3000
		if end > len(js) {
			end = len(js)
		}
		body := js[openIdx2:end]
		if !strings.Contains(body, "if (cronDetailJobId === jobId)") {
			t.Error("openCronDetail: must short-circuit when cronDetailJobId === jobId already (avoid redundant openCronPanel + fetchCronJobs + focus-steal on repeated clicks)")
		}
	}
}

// TestDashboardHTML_CronPanelConsolidationStyles pins the CSS shell that the
// drawer needs. If any of these classes are deleted by a future "dead CSS
// cleanup" pass, the drawer will render but look broken.
func TestDashboardHTML_CronPanelConsolidationStyles(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(data)

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
