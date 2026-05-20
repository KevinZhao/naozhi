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
		end := openIdx + 2000
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
		if strings.Count(body, "renderCronPanel()") > 0 {
			// note: openCronPanel call is fine; the redundant explicit
			// renderCronPanel() call inside openCronDetail's body must be gone.
			renderCount := strings.Count(body, "renderCronPanel()")
			if renderCount > 0 {
				t.Errorf("openCronDetail: must not call renderCronPanel() directly (redundant — openCronPanel already does); found %d call(s)", renderCount)
			}
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

// TestDashboardJS_R2_R4_TriggerCooldown pins the Round 2 review R-4 disable
// matrix for the drawer's "▷ 立即执行" button (RFC §4.3.1). The behaviour
// is a four-state machine — normal / paused / running / just-triggered —
// with the latter sub-divided into spinner (≤1s) → ✓ (1-3s) → quiet hold
// (3-10s). Naive "fire-and-forget + toast" lets operators reflexively
// double-click during the WS round-trip; the cooldown is what stops that.
func TestDashboardJS_R2_R4_TriggerCooldown(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// 1. cronJustTriggered tracker must exist as a module-scoped map.
	if !strings.Contains(js, "const cronJustTriggered = Object.create(null)") {
		t.Error("dashboard.js: cronJustTriggered map must be module-scoped — drawer + future list-row sync rely on a single source of truth")
	}

	// 2. 10 s debounce floor must match the documented behaviour.
	if !strings.Contains(js, "const CRON_TRIGGER_COOLDOWN_MS = 10 * 1000") {
		t.Error("dashboard.js: CRON_TRIGGER_COOLDOWN_MS must be 10s — RFC §4.3.1 disable matrix")
	}

	// 3. State helper returns the right phase labels.
	if !strings.Contains(js, "function cronTriggerCooldownState(id)") {
		t.Error("dashboard.js: cronTriggerCooldownState(id) helper missing")
	}
	if !strings.Contains(js, "{ phase: 'sending', label: '触发中…' }") {
		t.Error("dashboard.js: cooldown phase 'sending' must surface label '触发中…' (≤1s window)")
	}
	if !strings.Contains(js, "{ phase: 'sent',    label: '已派发 ✓' }") {
		t.Error("dashboard.js: cooldown phase 'sent' must surface label '已派发 ✓' (1-3s success window)")
	}

	// 4. cronTriggerNow must (a) reentrancy-guard, (b) optimistic-lock
	// before fetch, (c) clear cooldown on non-2xx and on network errors.
	trigIdx := strings.Index(js, "async function cronTriggerNow(id)")
	if trigIdx < 0 {
		t.Fatal("dashboard.js: cronTriggerNow not found")
	}
	trigEnd := trigIdx + 4000
	if trigEnd > len(js) {
		trigEnd = len(js)
	}
	trigBody := js[trigIdx:trigEnd]
	if !strings.Contains(trigBody, "if (cronJustTriggered[id]) return") {
		t.Error("cronTriggerNow: must reentrancy-guard on the cronJustTriggered map (drops same-id double-click silently)")
	}
	if !strings.Contains(trigBody, "cronJustTriggered[id] = Date.now()") {
		t.Error("cronTriggerNow: must set cronJustTriggered[id] BEFORE the fetch so the spinner appears synchronously")
	}
	if !strings.Contains(trigBody, "ensureCronTriggerCooldownTick()") {
		t.Error("cronTriggerNow: must start the cooldown tick timer so label transitions sending→sent→idle even without further events")
	}
	if !strings.Contains(trigBody, "cronTriggerCooldownClear(id)") {
		t.Error("cronTriggerNow: must call cronTriggerCooldownClear(id) on error paths so users can retry without waiting 10s on a transient 502")
	}

	// 5. WS cron_run_started clears cooldown so the disabled-running label
	//    takes over (the running-state has higher precedence than the
	//    optimistic local lock).
	startedIdx := strings.Index(js, "function cronApplyRunStarted(msg)")
	if startedIdx < 0 {
		t.Fatal("dashboard.js: cronApplyRunStarted not found")
	}
	startedEnd := startedIdx + 2500
	if startedEnd > len(js) {
		startedEnd = len(js)
	}
	startedBody := js[startedIdx:startedEnd]
	if !strings.Contains(startedBody, "cronTriggerCooldownClear(msg.job_id)") {
		t.Error("cronApplyRunStarted: must cronTriggerCooldownClear(msg.job_id) so the WS-confirmed running state replaces the optimistic cooldown")
	}

	// 6. Drawer trigger button reads cronTriggerCooldownState and emits
	//    is-sending / is-sent CSS classes when in cooldown.
	drawerIdx := strings.Index(js, "function cronDrawerHtml(j)")
	if drawerIdx < 0 {
		// The drawer renderer might be inlined into renderCronDrawer in
		// older revisions; search for either name.
		drawerIdx = strings.Index(js, "function renderCronDrawer()")
	}
	if drawerIdx < 0 {
		t.Fatal("dashboard.js: cronDrawerHtml / renderCronDrawer not found")
	}
	// The cooldown read should land near the action row; search a wider
	// window so the test stays robust to renderCronDrawer being split into
	// helper functions.
	const drawerLookahead = 12000
	drawerEnd := drawerIdx + drawerLookahead
	if drawerEnd > len(js) {
		drawerEnd = len(js)
	}
	drawerBody := js[drawerIdx:drawerEnd]
	if !strings.Contains(drawerBody, "cronTriggerCooldownState(id)") {
		t.Error("cron drawer: action row must consult cronTriggerCooldownState(id) when computing trigger button label/class")
	}
	if !strings.Contains(drawerBody, "is-sending") || !strings.Contains(drawerBody, "is-sent") {
		t.Error("cron drawer: cooldown sending/sent classes must drive the spinner + ✓ visual feedback")
	}

	// 7. CSS rules for is-sending / is-sent / is-running must exist.
	htmlData, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(htmlData)
	for _, rule := range []string{
		".cron-drawer-actions .cda-btn.primary.is-running",
		".cron-drawer-actions .cda-btn.primary.is-sending",
		".cron-drawer-actions .cda-btn.primary.is-sent",
		"@keyframes cdaSpinner",
	} {
		if !strings.Contains(html, rule) {
			t.Errorf("dashboard.html: missing CSS rule %q for R-4 trigger feedback", rule)
		}
	}
	// reduced-motion override on the spinner is required for a11y.
	if !strings.Contains(html, "prefers-reduced-motion:reduce") || !strings.Contains(html, "is-sending::before{animation:none}") {
		t.Error("dashboard.html: prefers-reduced-motion must disable the cdaSpinner / is-running pulse to comply with §11 a11y")
	}
}

// TestDashboardJS_R2_R1_LayoutObserver pins the Round 2 review R-1 fix:
// responsive layout for the cron panel keys off the *main column width*
// (ResizeObserver) instead of viewport @media. Three tier breakpoints
// + single-column collapse must be wired so a 1080p user with a wide
// sidebar doesn't get a 240px-wide drawer.
func TestDashboardJS_R2_R1_LayoutObserver(t *testing.T) {
	t.Parallel()
	jsData, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(jsData)
	htmlData, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
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

	// 2. Three breakpoints (1100 / 820 / 560) must appear in the JS.
	for _, threshold := range []string{"1100", "820", "560"} {
		if !strings.Contains(js, "w >= "+threshold) {
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
