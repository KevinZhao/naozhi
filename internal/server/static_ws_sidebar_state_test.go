package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Contract tests for the dashboard WS receive table + sidebar state
// handling. They read dashboard.js source (same approach as
// static_event_uuid_dedup_test.go) so a regression in any of these
// branches fails `go test` rather than only surfacing in a browser.

// wsOnHandler returns the source of the wsm.on(...) call for type key: the
// unconditional registration, or with claim set the first one passing a claim
// (a third argument). Bracket depth bounds the call; the scan does not skip
// string contents, which the registrations do not need.
func wsOnHandler(t *testing.T, js, key string, claim bool) string {
	t.Helper()
	head := "wsm.on(NZ_CONTRACT.WS." + key + ","
	for from := 0; ; {
		i := strings.Index(js[from:], head)
		if i < 0 {
			t.Fatalf("no wsm.on registration for %s (claim=%v)", key, claim)
		}
		start := from + i
		depth, commas, end := 0, 0, -1
		for j := start + len("wsm.on"); j < len(js) && end < 0; j++ {
			switch js[j] {
			case '(', '{', '[':
				depth++
			case ')', '}', ']':
				depth--
				if depth == 0 {
					end = j + 1
				}
			case ',':
				if depth == 1 {
					commas++
				}
			}
		}
		if end < 0 {
			t.Fatalf("unbounded wsm.on registration for %s", key)
		}
		if (commas == 2) == claim {
			return js[start:end]
		}
		from = end
	}
}

// TestDashboardJS_WSSwitchCoversBackendFrameTypes was here. It required every
// type in internal/wsproto/wsproto.schema.json to have a `case '<type>':` in
// wsm.onMessage. test/e2e/check-ws-contract.mjs does the same thing and more —
// it checks BOTH directions, so a frontend case for a type the backend does not
// declare also fails — and it runs in the lint-js CI job. Probed: renaming
// `case 'daemon_run_started':` failed both, so the Go copy added nothing (#2547).

// TestDashboardJS_DaemonRunFramesRefreshSystemDaemons pins that the sysession
// branch of the unified run_started/run_ended dispatch (#2540) actually
// re-fetches daemon state (the only path that updates the rail badge) rather
// than being an empty no-op. The fetch itself is behaviour-tested in
// test/e2e/run_frames_unified.test.js; what only this anchor pins is the
// hidden-tab suspension below, which the e2e suite does not drive.
func TestDashboardJS_DaemonRunFramesRefreshSystemDaemons(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	for _, reg := range []string{
		"const sysRun = (msg) => msg.subsystem === 'sysession';",
		"wsm.on(NZ_CONTRACT.WS.run_started, daemonRun, sysRun);",
		"wsm.on(NZ_CONTRACT.WS.run_ended, daemonRun, sysRun);",
	} {
		if !strings.Contains(js, reg) {
			t.Errorf("sysession run frames must be claimed by daemonRun: missing %q", reg)
		}
	}
	idx := strings.Index(js, "const daemonRun = () => {")
	if idx < 0 {
		t.Fatal("daemonRun handler missing")
	}
	daemonCase := js[idx:]
	end := strings.Index(daemonCase, "\n};")
	if end < 0 {
		t.Fatal("daemonRun handler has no closing `};` line")
	}
	daemonCase = daemonCase[:end]
	if !strings.Contains(daemonCase, "fetchSystemDaemons()") {
		t.Error("sysession run frames must call fetchSystemDaemons() so updateSystemBadge runs")
	}
	// Hub-wide broadcast, ~4 frames/min/tab: must honour the RNEW-UX-014
	// hidden-tab suspension instead of fetching in the background.
	if !strings.HasPrefix(strings.TrimSpace(daemonCase[len("const daemonRun = () => {"):]), "if (document.hidden) return;") {
		t.Error("daemonRun must return before fetching while document.hidden")
	}
	// ...and startPollers must re-sync the badge once the tab is visible again.
	startIdx := strings.Index(js, "const startPollers = () => {")
	visIdx := strings.Index(js, "document.addEventListener('visibilitychange'")
	if startIdx < 0 || visIdx < startIdx {
		t.Fatal("startPollers / visibilitychange listener not found in expected order")
	}
	if !strings.Contains(js[startIdx:visIdx], "fetchSystemDaemons()") {
		t.Error("startPollers must call fetchSystemDaemons() to catch up on daemon_run_* frames ignored while hidden")
	}
}

// TestDashboardJS_InterruptAckSurfacesStatus pins that interrupt_ack is no
// longer a silent no-op: the send path optimistically toasts "已发送中断", so
// a status:"error" (unknown node / server shutting down / internal error)
// or "not_running" ack must be surfaced or the operator believes the
// interrupt landed.
func TestDashboardJS_InterruptAckSurfacesStatus(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	if got := wsOnHandler(t, js, "interrupt_ack", false); got != "wsm.on(NZ_CONTRACT.WS.interrupt_ack, (msg) => sessionFrames.onInterruptAck(msg))" {
		t.Errorf("interrupt_ack must dispatch to sessionFrames.onInterruptAck(msg) instead of a no-op, got %q", got)
	}
	ack := jsMethodBody(t, js, "onInterruptAck")
	if !strings.Contains(ack, "showAPIError('中断会话', 500, msg.error || '')") {
		t.Error("onInterruptAck must toast status:'error' acks via showAPIError('中断会话', 500, msg.error || '')")
	}
	if !strings.Contains(ack, "msg.status === 'not_running'") {
		t.Error("onInterruptAck must handle status:'not_running' with a distinct hint")
	}
}

// TestDashboardJS_ErrorFrameGuardsPendingSubscribeKey pins that a generic
// `error` frame only clears the pending-subscribe bookkeeping when it is
// about that subscription. agent_subscribe validation failures and the
// node-disconnect broadcast (error{node, "node disconnected"}) are also
// `error` frames and used to wipe an unrelated in-flight subscribe.
func TestDashboardJS_ErrorFrameGuardsPendingSubscribeKey(t *testing.T) {
	t.Parallel()
	errCase := wsOnHandler(t, readDashboardJS(t), "error", false)
	if !strings.Contains(errCase, "msg.key === wsm._pendingSubscribeKey") {
		t.Error("error case must compare msg.key against wsm._pendingSubscribeKey before clearing pending state")
	}
	if !strings.Contains(errCase, "if (!msg.key && msg.node && msg.error === 'node disconnected')") {
		t.Error("error case must recognise the PurgeNodeSubscriptions frame (keyless + msg.node + 'node disconnected')")
	}
	if !strings.Contains(errCase, "reconcileSelectedNode()") {
		t.Error("node-disconnected error must run reconcileSelectedNode() so selection.node snaps back to local")
	}
}

// TestDashboardJS_PreviewDiscoveredGenerationGuard pins the generation
// counter that stops two rapid previewDiscovered() calls from racing: the
// first call's awaited fetch used to render into whichever #events-scroll
// was current and start a second setInterval without clearing the first.
func TestDashboardJS_PreviewDiscoveredGenerationGuard(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	start := strings.Index(js, "async function previewDiscovered(")
	if start < 0 {
		t.Fatal("previewDiscovered not found")
	}
	// Bound the function at its column-0 closing brace (#2557 PR-E3 removed
	// the dead handleTakeoverClick that used to follow it).
	end := strings.Index(js[start:], "\n}")
	if end < 0 {
		t.Fatal("previewDiscovered body end not found")
	}
	fn := js[start : start+end]
	if !strings.Contains(js, "  previewGen: 0,") {
		t.Error("transcript.previewGen counter must be declared in state.js")
	}
	// The bump lives in stopPreviewPolling() so EVERY caller that moves the
	// operator off the discovered panel (selectSession, the createSession
	// paths, a newer preview) invalidates an in-flight preview fetch — the
	// managed-session panel reuses the #events-scroll id, so an element
	// existence check alone cannot tell the two apart.
	stopIdx := strings.Index(js, "function stopPreviewPolling() {")
	if stopIdx < 0 {
		t.Fatal("stopPreviewPolling not found")
	}
	stopFn := js[stopIdx:]
	if e := strings.Index(stopFn, "\n}\n"); e > 0 {
		stopFn = stopFn[:e]
	}
	if !strings.Contains(stopFn, "transcript.previewGen++;") {
		t.Error("stopPreviewPolling() must bump transcript.previewGen so selectSession/createSession invalidate in-flight previews")
	}
	if !strings.Contains(fn, "deps.stopPreviewPolling();\n  const gen = transcript.previewGen;") {
		t.Error("previewDiscovered must call stopPreviewPolling() FIRST and then capture gen = transcript.previewGen (capturing before the call would be invalidated by its own bump)")
	}
	if strings.Contains(fn, "++transcript.previewGen") {
		t.Error("previewDiscovered must not bump transcript.previewGen itself — the bump belongs to stopPreviewPolling()")
	}
	if strings.Count(fn, "if (gen !== transcript.previewGen) return;") < 3 {
		t.Error("previewDiscovered must check the generation after the awaited fetch (ok + error paths) AND inside the poll tick")
	}
	idxSet := strings.Index(fn, "timers.preview = setInterval(")
	if idxSet < 0 {
		t.Fatal("timers.preview = setInterval( not found")
	}
	idxGen := strings.Index(fn, "if (gen !== transcript.previewGen) return;\n    const el = document.getElementById('events-scroll');")
	if idxGen < 0 || idxGen > idxSet {
		t.Fatal("generation check must precede the #events-scroll lookup and timers.preview = setInterval(")
	}
	// Between the post-fetch generation check and arming the interval there
	// must be NO stopPreviewPolling() call: it bumps _previewGen and would
	// invalidate this very call (its tick would bail on the first fire). The
	// prologue call already cleared any older generation's interval.
	if strings.Contains(fn[idxGen:idxSet], "deps.stopPreviewPolling();") {
		t.Error("previewDiscovered must not call stopPreviewPolling() between the gen check and setInterval — it would invalidate its own generation")
	}
}

// TestDashboardJS_HistoryPanelGroupKeyMatchesSortKey pins that the history
// popover's day-header grouping uses the same timestamp as its sort. Sorting
// by `retired_at || last_active` while grouping by `last_active` produced
// repeated / out-of-order day headers whenever a session was retired on a
// later day than its last activity.
func TestDashboardJS_HistoryPanelGroupKeyMatchesSortKey(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	if !strings.Contains(js, "merged.sort((a, b) => (b.retired_at || b.last_active) - (a.retired_at || a.last_active));") {
		t.Fatal("history sort key changed — update this test alongside the grouping key")
	}
	start := strings.Index(js, "function applyHistoryFilter(")
	if start < 0 {
		t.Fatal("applyHistoryFilter not found")
	}
	fn := js[start:]
	if end := strings.Index(fn, "\n}\n"); end > 0 {
		fn = fn[:end]
	}
	if !strings.Contains(fn, "const groupTs = s.retired_at || s.last_active;") {
		t.Error("applyHistoryFilter must group day headers by `s.retired_at || s.last_active` (same key as the sort)")
	}
	if strings.Contains(fn, "const d = new Date(s.last_active);") {
		t.Error("applyHistoryFilter still groups by s.last_active alone")
	}
}

// TestDashboardJS_SystemStatLabelsMatchAutoTitlerSkipReasons pins the
// SYSTEM_STAT_LABELS keys against the bumpSkip(...) reasons AutoTitler
// actually emits (flattenTickReport prefixes them with "skipped_"). The
// label table carried `skipped_min_user_turns` while the daemon emits
// `min_first_turns`, so that bucket fell through to the raw-suffix fallback.
func TestDashboardJS_SystemStatLabelsMatchAutoTitlerSkipReasons(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	src, err := os.ReadFile(filepath.Join("..", "sysession", "auto_titler.go"))
	if err != nil {
		t.Fatalf("read auto_titler.go: %v", err)
	}
	re := regexp.MustCompile(`bumpSkip\("([a-z_]+)"\)`)
	reasons := re.FindAllStringSubmatch(string(src), -1)
	if len(reasons) == 0 {
		t.Fatal("no bumpSkip(...) reasons found in auto_titler.go — extraction broken")
	}
	start := strings.Index(js, "const SYSTEM_STAT_LABELS = {")
	if start < 0 {
		t.Fatal("SYSTEM_STAT_LABELS not found")
	}
	table := js[start:]
	if end := strings.Index(table, "};"); end > 0 {
		table = table[:end]
	}
	for _, m := range reasons {
		key := "skipped_" + m[1] + ":"
		if !strings.Contains(table, key) {
			t.Errorf("SYSTEM_STAT_LABELS lacks %q — AutoTitler emits bumpSkip(%q)", key, m[1])
		}
	}
}

// TestDashboardJS_SidebarRelativeTimeTick pins the 60s ticker that refreshes
// the "2m ago" labels on session cards. While WS is connected renderSidebar
// only runs on sessions_update, so the relative time froze at whatever the
// last render produced. The tick must be pausable via the visibilitychange
// gate like the other pollers.
func TestDashboardJS_SidebarRelativeTimeTick(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	if !strings.Contains(js, `class="sc-time"' + (absTime ? ' title="' + escAttr(absTime) + '"' : '') + ' data-ts="' + s.last_active + '">'`) {
		t.Error("session card .sc-time must carry data-ts (after the title attr) so the ticker can recompute the label without a re-render")
	}
	if !strings.Contains(js, "function refreshSidebarTimes() {") {
		t.Fatal("refreshSidebarTimes() must exist")
	}
	if !strings.Contains(js, "function startSidebarTimeTick() {") || !strings.Contains(js, "function stopSidebarTimeTick() {") {
		t.Fatal("startSidebarTimeTick()/stopSidebarTimeTick() must exist")
	}
	if !strings.Contains(js, "setInterval(refreshSidebarTimes, SIDEBAR_TIME_TICK_MS)") {
		t.Error("ticker must be armed with setInterval(refreshSidebarTimes, SIDEBAR_TIME_TICK_MS)")
	}
	if !strings.Contains(js, "const SIDEBAR_TIME_TICK_MS = 60000;") {
		t.Error("SIDEBAR_TIME_TICK_MS must be 60000 (labels have 1-minute resolution)")
	}
	// Visibility gate: stopPollers must stop it, startPollers must re-arm it.
	stopIdx := strings.Index(js, "const stopPollers = () => {")
	startIdx := strings.Index(js, "const startPollers = () => {")
	if stopIdx < 0 || startIdx < 0 || startIdx < stopIdx {
		t.Fatal("stopPollers/startPollers visibility gate not found in expected order")
	}
	if !strings.Contains(js[stopIdx:startIdx], "stopSidebarTimeTick();") {
		t.Error("stopPollers must call stopSidebarTimeTick() when the tab is hidden")
	}
	visIdx := strings.Index(js[startIdx:], "document.addEventListener('visibilitychange'")
	if visIdx < 0 {
		t.Fatal("visibilitychange listener not found after startPollers")
	}
	if !strings.Contains(js[startIdx:startIdx+visIdx], "startSidebarTimeTick();") {
		t.Error("startPollers must call startSidebarTimeTick() when the tab becomes visible")
	}
}
