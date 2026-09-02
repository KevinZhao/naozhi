package server

import (
	"regexp"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cron"
)

// cronViewDisplayJS loads cron_view.js once per test so each assertion below
// can scope itself to a function body. Mirrors static_cron_timeline_cap_test.go.
func cronViewDisplayJS(t *testing.T) string {
	t.Helper()
	data, err := cronViewJS.ReadFile("static/cron_view.js")
	if err != nil {
		t.Fatalf("read cron_view.js: %v", err)
	}
	return string(data)
}

// cronViewFnBody returns a bounded slice of js starting at the given function
// signature. Fatal when the signature is missing so a rename shows up as a
// clear failure instead of a silently-empty body.
func cronViewFnBody(t *testing.T, js, sig string, span int) string {
	t.Helper()
	idx := strings.Index(js, sig)
	if idx < 0 {
		t.Fatalf("cron_view.js: %q not found", sig)
	}
	end := idx + span
	if end > len(js) {
		end = len(js)
	}
	return js[idx:end]
}

// TestCronViewJS_RecentRunsSeedUsesServerCap pins bug #1 of the cron-view
// display fix: renderCronTimelineForJob seeded st.done from a hard-coded
// `recent_runs.length < 10` while the backend embeds only recentRunsPerJob
// (5) summaries per job — so every job with ≥5 runs rendered 5 rows plus a
// disabled 已到结尾 and 加载更多 was unreachable. The cap must now come from
// the list response (recent_runs_cap) instead of a second front-end literal.
func TestCronViewJS_RecentRunsSeedUsesServerCap(t *testing.T) {
	t.Parallel()
	js := cronViewDisplayJS(t)

	if strings.Contains(js, "recent_runs.length < 10") {
		t.Error("cron_view.js: hard-coded `recent_runs.length < 10` must be gone — the backend caps recent_runs at recentRunsPerJob (5), so this made 加载更多 unreachable")
	}
	if !strings.Contains(js, "let cronRecentRunsCap") {
		t.Error("cron_view.js: must keep a module-level cronRecentRunsCap fed from the list response")
	}
	fetchBody := cronViewFnBody(t, js, "async function fetchCronJobs()", 3000)
	if !strings.Contains(fetchBody, "data.recent_runs_cap") {
		t.Error("fetchCronJobs: must read data.recent_runs_cap from /api/cron")
	}
	seedBody := cronViewFnBody(t, js, "function renderCronTimelineForJob(jobId)", 2500)
	if !strings.Contains(seedBody, "cronRecentRunsCap") {
		t.Error("renderCronTimelineForJob: st.done seeding must compare against cronRecentRunsCap, not a literal")
	}
	if !strings.Contains(seedBody, "total <= st.runs.length") {
		t.Error("renderCronTimelineForJob: when stats.total <= rows already in hand the list is complete — skip the empty /api/cron/runs round-trip")
	}
}

// TestCronViewJS_TimelineDetailClickGuard pins bug #2: the .ctr-detail
// block is nested inside the .ctr row whose onclick is cronTimelineSelectRun
// — which collapses the row when it is already expanded. Any click inside the
// detail (输入快照 <details>, ↩ 重放自, text selection) therefore folded the
// row. Both row handlers (click + Enter/Space keydown) must ignore events
// originating inside .ctr-detail. The guard lives in the row handler (not an
// extra onclick on the detail) so the CSP inline-onclick ratchet stays flat.
func TestCronViewJS_TimelineDetailClickGuard(t *testing.T) {
	t.Parallel()
	js := cronViewDisplayJS(t)
	body := cronViewFnBody(t, js, "function cronTimelineRowHtml(", 6000)
	idx := strings.Index(body, `'<div class="ctr' +`)
	if idx < 0 {
		t.Fatal("cronTimelineRowHtml: .ctr row opening tag not found")
	}
	open := body[idx:]
	if end := strings.Index(open, `'<div class="ctr-main">'`); end > 0 {
		open = open[:end]
	}
	if !strings.Contains(body, `const detailGuard = '!event.target.closest(\'.ctr-detail\')'`) {
		t.Error("cronTimelineRowHtml: must define detailGuard = !event.target.closest('.ctr-detail')")
	}
	const guard = "' + detailGuard + '"
	onclickIdx := strings.Index(open, `onclick="`)
	onkeyIdx := strings.Index(open, `onkeydown="`)
	if onclickIdx < 0 || onkeyIdx < 0 {
		t.Fatalf(".ctr row must carry both onclick and onkeydown; got %q", open)
	}
	if !strings.Contains(open[onclickIdx:onkeyIdx], guard) {
		t.Errorf(".ctr row onclick must guard with %s so clicks inside the detail don't collapse the row; got %q", guard, open[onclickIdx:onkeyIdx])
	}
	if !strings.Contains(open[onkeyIdx:], guard) {
		t.Errorf(".ctr row onkeydown must guard with %s (Enter/Space inside the detail must not toggle the row); got %q", guard, open[onkeyIdx:])
	}
	// The detail wrapper itself must NOT grow an inline onclick (ratchet).
	if dIdx := strings.Index(body, `'<div class="ctr-detail"`); dIdx >= 0 {
		dOpen := body[dIdx:]
		if end := strings.Index(dOpen, ">' +"); end > 0 {
			dOpen = dOpen[:end]
		}
		if strings.Contains(dOpen, "onclick=") {
			t.Errorf(".ctr-detail must not add an inline onclick (CSP ratchet); use the row-handler guard instead; got %q", dOpen)
		}
	}
}

// TestCronViewJS_ServerTimezoneSurfaced pins bug #3: /api/cron returns
// timezone / timezone_abbr / timezone_label but the front end ignored them,
// so a browser in another zone saw "每天 06:13" next to a next_run rendered
// in browser-local time. The list fetch must stash the server zone and the
// schedule surfaces (card chip / drawer 什么时候 / editor freq-hint) must
// annotate when the browser zone differs.
func TestCronViewJS_ServerTimezoneSurfaced(t *testing.T) {
	t.Parallel()
	js := cronViewDisplayJS(t)

	fetchBody := cronViewFnBody(t, js, "async function fetchCronJobs()", 3000)
	for _, want := range []string{"data.timezone", "data.timezone_abbr", "data.timezone_label"} {
		if !strings.Contains(fetchBody, want) {
			t.Errorf("fetchCronJobs: must read %s from the list response", want)
		}
	}
	if !strings.Contains(js, "function cronTimezoneSuffix(") {
		t.Fatal("cron_view.js: cronTimezoneSuffix helper must exist (returns '' when browser zone matches the server zone)")
	}
	// time.Local schedulers report loc.String()=="Local" with an empty abbr;
	// neither may leak into the UI — fall back to the UTC±HH:MM offset.
	if !strings.Contains(js, "cronTimezone !== 'Local'") {
		t.Error("cron_view.js: timezone name 'Local' must be treated as no-name (fall back to the UTC offset from timezone_label)")
	}
	if !strings.Contains(js, "function cronTimezoneUTCTag(") {
		t.Error("cron_view.js: cronTimezoneUTCTag helper must extract UTC±HH:MM from timezone_label for the fallback")
	}
	for _, fn := range []string{"function cronTimezoneSuffix()", "function cronTimezoneNote()"} {
		if !strings.Contains(cronViewFnBody(t, js, fn, 600), "cronTimezoneUTCTag()") {
			t.Errorf("%s must fall back to cronTimezoneUTCTag() when abbr/name are unusable", fn)
		}
	}
	for _, fn := range []string{
		"function cronJobCardHtml(j)",
		"function cronDrawerSpecHtml(j)",
	} {
		if !strings.Contains(cronViewFnBody(t, js, fn, 6000), "cronTimezoneSuffix(") {
			t.Errorf("%s must annotate the schedule text via cronTimezoneSuffix()", fn)
		}
	}
	hintIdx := strings.Index(js, `class="freq-hint"`)
	if hintIdx < 0 {
		t.Fatal("cron_view.js: freq-hint not found")
	}
	if !strings.Contains(js[hintIdx:hintIdx+400], "cronTimezoneNote(") {
		t.Error("editor freq-hint must append cronTimezoneNote() so the picker's HH:MM is labelled with the server zone when it differs from the browser")
	}
}

// TestCronViewJS_StatsBadgeOkBranchReachable pins bug #4: cronStatsBadgeHtml
// compared `stats.failed === 0` but the backend marshals the counters with
// omitempty, so a perfectly healthy job had failed === undefined and never
// took the green branch.
func TestCronViewJS_StatsBadgeOkBranchReachable(t *testing.T) {
	t.Parallel()
	js := cronViewDisplayJS(t)
	body := cronViewFnBody(t, js, "function cronStatsBadgeHtml(j)", 2500)
	if strings.Contains(body, "stats.failed === 0") || strings.Contains(body, "stats.timed_out === 0") {
		t.Error("cronStatsBadgeHtml: strict `=== 0` against omitempty counters is never true when the field is absent — coerce with `|| 0`")
	}
	if !strings.Contains(body, "(stats.failed || 0) === 0") || !strings.Contains(body, "(stats.timed_out || 0) === 0") {
		t.Error("cronStatsBadgeHtml: ok branch must use `(stats.failed || 0) === 0 && (stats.timed_out || 0) === 0`")
	}
}

// TestCronViewJS_CooldownTickNoFullDrawerRebuild pins bug #5: the 200 ms
// cooldown tick after 立即执行 rebuilt the whole drawer (`anyExpired || true`)
// for 10 s, wiping text selection and <details> state. The tick must only
// patch the trigger button inside .cron-drawer-actions.
func TestCronViewJS_CooldownTickNoFullDrawerRebuild(t *testing.T) {
	t.Parallel()
	js := cronViewDisplayJS(t)
	body := cronViewFnBody(t, js, "function ensureCronTriggerCooldownTick()", 3000)
	if cut := strings.Index(body, "\nasync function cronTriggerNow("); cut > 0 {
		body = body[:cut]
	}
	if strings.Contains(body, "|| true") {
		t.Error("ensureCronTriggerCooldownTick: `anyExpired || true` forces a full renderCronDrawer every 200 ms — remove it")
	}
	if strings.Contains(body, "renderCronDrawer()") {
		t.Error("ensureCronTriggerCooldownTick: must not call renderCronDrawer() from the tick; patch the trigger button in place instead")
	}
	if !strings.Contains(body, "cronDrawerRefreshTriggerBtn(") {
		t.Error("ensureCronTriggerCooldownTick: must call cronDrawerRefreshTriggerBtn() to update only the action button")
	}
	if !strings.Contains(js, "function cronDrawerRefreshTriggerBtn(") {
		t.Error("cron_view.js: cronDrawerRefreshTriggerBtn helper must exist")
	}
}

// TestCronViewJS_AttentionChipVisible pins bug #6: the rail badge said 需关注 N
// but inside the panel the only trace was a `hidden` summary span and the
// status chips offered just 全部 / 运行中 even though setCronStatusFilter
// already supported 'attention'.
func TestCronViewJS_AttentionChipVisible(t *testing.T) {
	t.Parallel()
	js := cronViewDisplayJS(t)
	body := cronViewFnBody(t, js, "function renderCronPanel()", 12000)
	// Chips share one template (statusChip) so adding 需关注 doesn't raise the
	// CSP inline-onclick ratchet; the template must wire data-status + onclick.
	tmpl := regexp.MustCompile(`class="cron-status-chip[^>]*data-status="' \+ status \+ '"[^>]*onclick="setCronStatusFilter\(\\'' \+ status \+ '\\'\)"`)
	if !tmpl.MatchString(body) {
		t.Error("renderCronPanel: statusChip template must render data-status + onclick=setCronStatusFilter(status)")
	}
	if !strings.Contains(body, "statusChip('attention', '需关注 ' + attentionCount") {
		t.Error("renderCronPanel: status chips must include a 需关注 N chip via statusChip('attention', ...)")
	}
	if !strings.Contains(body, "attentionCount > 0") {
		t.Error("renderCronPanel: the 需关注 chip / filter bar must be gated on attentionCount > 0")
	}
}

// TestCronViewJS_ShowAllPersistsInState pins bug #7: the 查看全部 expansion
// lived only in a DOM data-collapsed attribute, so any renderCronTimelinePanel
// (WS refresh, detail fetch landing, row select) folded the list back to 5.
func TestCronViewJS_ShowAllPersistsInState(t *testing.T) {
	t.Parallel()
	js := cronViewDisplayJS(t)
	stateBody := cronViewFnBody(t, js, "function getCronTimelineState(jobId)", 2500)
	if !strings.Contains(stateBody, "showAll: false") {
		t.Error("getCronTimelineState: per-job state must carry showAll: false")
	}
	htmlBody := cronViewFnBody(t, js, "function cronTimelineHtml(", 4000)
	if !strings.Contains(htmlBody, "st.showAll") {
		t.Error("cronTimelineHtml: data-collapsed must derive from st.showAll, not runs.length alone")
	}
	toggleBody := cronViewFnBody(t, js, "function cronTimelineToggleShowAll(btn)", 1500)
	if !strings.Contains(toggleBody, "showAll = true") {
		t.Error("cronTimelineToggleShowAll: must persist st.showAll = true")
	}
	expandBody := cronViewFnBody(t, js, "function cronTimelineExpand(jobId, runId)", 1500)
	if !strings.Contains(expandBody, "showAll = true") {
		t.Error("cronTimelineExpand: expanding a row past the fold must set st.showAll = true so the re-render doesn't hide it")
	}
}

// TestCronViewJS_PhaseAndErrorClassEnumsMatchBackend pins bug #8: the phase
// label switch used dispatch/send/waiting while the scheduler emits the
// cron.Phase* constants, and cronErrorClassLabel lacked two cron.ErrClass*
// values. Compare directly against the Go constants so drift fails here.
func TestCronViewJS_PhaseAndErrorClassEnumsMatchBackend(t *testing.T) {
	t.Parallel()
	js := cronViewDisplayJS(t)

	phaseBody := cronViewFnBody(t, js, "function cronPhaseLabel(phase)", 1200)
	for _, p := range []string{cron.PhaseQueued, cron.PhaseJittering, cron.PhaseSpawning, cron.PhaseSending} {
		if !strings.Contains(phaseBody, "case '"+p+"':") {
			t.Errorf("cronPhaseLabel: missing case for backend phase %q", p)
		}
	}

	errBody := cronViewFnBody(t, js, "function cronErrorClassLabel(cls)", 2500)
	classes := []cron.ErrorClass{
		cron.ErrClassSessionError,
		cron.ErrClassSendError,
		cron.ErrClassDeadlineExceeded,
		cron.ErrClassCanceled,
		cron.ErrClassWorkDirUnreachable,
		cron.ErrClassWorkDirOutsideRoot,
		cron.ErrClassOverlapSkipped,
		cron.ErrClassRouterMissing,
		cron.ErrClassPausedConcurrent,
		cron.ErrClassDeletedConcurrent,
		cron.ErrClassPanic,
		cron.ErrClassSandboxFailed,
		cron.ErrClassSandboxTransport,
		cron.ErrClassSandboxUnavailable,
	}
	for _, c := range classes {
		if !strings.Contains(errBody, "case '"+string(c)+"':") {
			t.Errorf("cronErrorClassLabel: missing case for backend ErrorClass %q", c)
		}
	}
}
