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

// TestDashboardJS_R2_R4_TriggerCooldown pins the Round 2 review R-4 disable
// matrix for the drawer's "▷ 立即执行" button (RFC §4.3.1). The behaviour
// is a four-state machine — normal / paused / running / just-triggered —
// with the latter sub-divided into spinner (≤1s) → ✓ (1-3s) → quiet hold
// (3-10s). Naive "fire-and-forget + toast" lets operators reflexively
// double-click during the WS round-trip; the cooldown is what stops that.
func TestDashboardJS_R2_R4_TriggerCooldown(t *testing.T) {
	t.Parallel()
	data, err := cronViewJS.ReadFile("static/cron_view.js")
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
	htmlData := []byte(readDashboardHTMLAndCSS(t))
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

// TestDashboardJS_R2_R12_DeleteCopy pins the Round 2 review R-12 destructive-
// flow rewrite (RFC §7.5). Three branches are required:
//  1. Normal delete — references run count + JSONL preservation hint
//  2. Running delete — special copy explaining the in-flight execution
//     will continue but won't be recorded
//  3. Never-run delete — terse copy without history hint
//
// All three flow through a 3 s countdown via confirmDialog's countdownSecs.
func TestDashboardJS_R2_R12_DeleteCopy(t *testing.T) {
	t.Parallel()
	jsData, err := cronViewJS.ReadFile("static/cron_view.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(jsData)
	htmlData := []byte(readDashboardHTMLAndCSS(t))
	html := string(htmlData)

	// 1. confirmDialog must support countdownSecs option. Old call sites
	//    (boolean confirmText only) keep working because countdownSecs
	//    defaults to 0 = no countdown.
	if !strings.Contains(js, "countdownSecs") {
		t.Error("confirmDialog: must accept countdownSecs option for destructive flows (RFC §7.5)")
	}

	// 2. cronDelete must compute the running branch + non-running branch.
	delIdx := strings.Index(js, "async function cronDelete(id)")
	if delIdx < 0 {
		t.Fatal("dashboard.js: cronDelete not found")
	}
	delEnd := delIdx + 5000
	if delEnd > len(js) {
		delEnd = len(js)
	}
	delBody := js[delIdx:delEnd]

	// Running branch — must mention "正在执行" + "继续运行直到完成" + "不会被记录".
	if !strings.Contains(delBody, "正在执行") ||
		!strings.Contains(delBody, "继续运行直到完成") ||
		!strings.Contains(delBody, "不会被记录") {
		t.Error("cronDelete: running branch must explain in-flight execution continues + result not recorded (RFC §7.5)")
	}

	// Normal branch — must mention JSONL + "claude --resume" but bury it
	// behind the "在终端" path, not the headline.
	if !strings.Contains(delBody, "JSONL") || !strings.Contains(delBody, "claude --resume") {
		t.Error("cronDelete: normal branch must mention JSONL + claude --resume revival path (RFC §7.5)")
	}
	// "在终端用" guards against the resume command being moved to the
	// headline (which would alarm L1 operators who don't shell).
	if !strings.Contains(delBody, "在终端用") {
		t.Error("cronDelete: claude --resume must be qualified with 在终端用 so non-shell users skip past it (RFC §7.5)")
	}

	// 3 s countdown wired.
	if !strings.Contains(delBody, "countdownSecs: 3") {
		t.Error("cronDelete: must pass countdownSecs: 3 (RFC §7.5 — 3 s is the documented friction floor)")
	}

	// Title must include the task name when available.
	if !strings.Contains(delBody, "删除「' + title + '」？") &&
		!strings.Contains(delBody, "'删除「' + title + '」？'") {
		t.Error("cronDelete: title must wrap the task name in 「」 when set so users see what they're deleting")
	}

	// 3. CSS — confirm-msg pre-wrap + countdown pulse + reduced-motion.
	if !strings.Contains(html, ".confirm-msg") || !strings.Contains(html, "white-space:pre-wrap") {
		t.Error("dashboard.html: .confirm-msg must use white-space:pre-wrap so multi-paragraph delete copy renders correctly")
	}
	if !strings.Contains(html, "@keyframes confirmCountdown") {
		t.Error("dashboard.html: countdown pulse keyframe missing")
	}
	if !strings.Contains(html, "button.danger[disabled]{animation:none}") {
		t.Error("dashboard.html: prefers-reduced-motion must disable the countdown pulse")
	}
}
