package server

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Contract tests for the dashboard WS dispatch table + sidebar state
// handling. They read dashboard.js source (same approach as
// static_event_uuid_dedup_test.go) so a regression in any of these
// branches fails `go test` rather than only surfacing in a browser.

// wsOnMessageBody extracts the body of wsm.onMessage(msg) — the WS dispatch
// switch — so assertions can be scoped to that function instead of the
// whole 15k-line file.
func wsOnMessageBody(t *testing.T, js string) string {
	t.Helper()
	start := strings.Index(js, "onMessage(msg) {")
	if start < 0 {
		t.Fatal("wsm.onMessage(msg) not found in dashboard.js")
	}
	end := strings.Index(js[start:], "startPing() {")
	if end < 0 {
		t.Fatal("wsm.startPing() must follow onMessage so the dispatch body can be sliced")
	}
	return js[start : start+end]
}

// backendWSFrameTypes scans every non-test wshub*.go file in this package
// and collects the `type` values the server can push to a dashboard client:
// struct literals of the form `...Msg{Type: "x"` (node.ServerMsg and the
// dedicated broadcast structs) plus the pre-encoded frame constants
// (`{"type":"x"}`) in wshub.go.
func backendWSFrameTypes(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("wshub*.go")
	if err != nil {
		t.Fatalf("glob wshub*.go: %v", err)
	}
	literalRe := regexp.MustCompile(`(?s)Msg\{\s*Type:\s*"([a-z_]+)"`)
	constRe := regexp.MustCompile(`\{"type":"([a-z_]+)"`)
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range literalRe.FindAllStringSubmatch(string(src), -1) {
			seen[m[1]] = true
		}
		for _, m := range constRe.FindAllStringSubmatch(string(src), -1) {
			seen[m[1]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) < 10 {
		t.Fatalf("backend frame-type extraction looks broken: only %d types found (%v)", len(out), out)
	}
	return out
}

// TestDashboardJS_WSSwitchCoversBackendFrameTypes pins that every frame type
// the hub can emit has a `case` in wsm.onMessage. daemon_run_started /
// daemon_run_ended were broadcast by wshub_broadcast.go but silently dropped
// by the dispatch switch, so the 系统 rail badge never refreshed on a daemon
// run boundary. Types the dashboard deliberately ignores go in the allowlist
// below — adding a new backend frame without a frontend case (or an explicit
// allowlist entry) fails this test.
func TestDashboardJS_WSSwitchCoversBackendFrameTypes(t *testing.T) {
	t.Parallel()
	body := wsOnMessageBody(t, readDashboardJS(t))
	ignored := map[string]string{
		// Server reply to an explicit unsubscribe; the client already
		// cleared its bookkeeping synchronously in wsm.unsubscribe().
		"unsubscribed": "client-side state already reset before the ack arrives",
	}
	for _, typ := range backendWSFrameTypes(t) {
		if _, ok := ignored[typ]; ok {
			continue
		}
		if !strings.Contains(body, "case '"+typ+"':") {
			t.Errorf("wsm.onMessage has no `case '%s':` but the hub emits that frame type", typ)
		}
	}
	// Explicit pins for the two frames this fix adds, so the failure message
	// names the regression directly.
	for _, typ := range []string{"daemon_run_started", "daemon_run_ended"} {
		if !strings.Contains(body, "case '"+typ+"':") {
			t.Errorf("wsm.onMessage must handle %s (refresh 系统 rail badge)", typ)
		}
	}
}

// TestDashboardJS_DaemonRunFramesRefreshSystemDaemons pins that the new
// daemon_run_* cases actually re-fetch daemon state (the only path that
// updates the rail badge) rather than being empty no-ops.
func TestDashboardJS_DaemonRunFramesRefreshSystemDaemons(t *testing.T) {
	t.Parallel()
	body := wsOnMessageBody(t, readDashboardJS(t))
	idx := strings.Index(body, "case 'daemon_run_ended':")
	if idx < 0 {
		t.Fatal("case 'daemon_run_ended' missing")
	}
	tail := body[idx:]
	end := strings.Index(tail, "case 'pong':")
	if end < 0 {
		t.Fatal("case 'pong' must follow daemon_run_ended")
	}
	daemonCase := tail[:end]
	if !strings.Contains(daemonCase, "fetchSystemDaemons()") {
		t.Error("daemon_run_* case must call fetchSystemDaemons() so updateSystemBadge runs")
	}
	// Hub-wide broadcast, ~4 frames/min/tab: must honour the RNEW-UX-014
	// hidden-tab suspension instead of fetching in the background.
	if !strings.Contains(daemonCase, "if (document.hidden) break;") {
		t.Error("daemon_run_* case must skip the fetch while document.hidden")
	}
	// ...and startPollers must re-sync the badge once the tab is visible again.
	js := readDashboardJS(t)
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
	body := wsOnMessageBody(t, js)
	idx := strings.Index(body, "case 'interrupt_ack':")
	if idx < 0 {
		t.Fatal("case 'interrupt_ack' missing")
	}
	tail := body[idx:]
	brk := strings.Index(tail, "break;")
	if brk < 0 {
		t.Fatal("interrupt_ack case has no break")
	}
	if !strings.Contains(tail[:brk], "this.onInterruptAck(msg)") {
		t.Error("interrupt_ack case must dispatch to this.onInterruptAck(msg) instead of a bare break")
	}
	if !strings.Contains(js, "onInterruptAck(msg) {") {
		t.Fatal("wsm.onInterruptAck(msg) handler must exist")
	}
	if !strings.Contains(js, "showAPIError('中断会话', 500, msg.error || '')") {
		t.Error("onInterruptAck must toast status:'error' acks via showAPIError('中断会话', 500, msg.error || '')")
	}
	if !strings.Contains(js, "msg.status === 'not_running'") {
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
	body := wsOnMessageBody(t, readDashboardJS(t))
	idx := strings.Index(body, "case 'error':")
	if idx < 0 {
		t.Fatal("case 'error' missing")
	}
	next := strings.Index(body[idx:], "case 'history':")
	if next < 0 {
		t.Fatal("case 'history' must follow case 'error'")
	}
	errCase := body[idx : idx+next]
	if !strings.Contains(errCase, "msg.key === this._pendingSubscribeKey") {
		t.Error("error case must compare msg.key against this._pendingSubscribeKey before clearing pending state")
	}
	if !strings.Contains(errCase, "if (!msg.key && msg.node && msg.error === 'node disconnected')") {
		t.Error("error case must recognise the PurgeNodeSubscriptions frame (keyless + msg.node + 'node disconnected')")
	}
	if !strings.Contains(errCase, "reconcileSelectedNode()") {
		t.Error("node-disconnected error must run reconcileSelectedNode() so selectedNode snaps back to local")
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
	end := strings.Index(js[start:], "function handleTakeoverClick(")
	if end < 0 {
		t.Fatal("handleTakeoverClick must follow previewDiscovered")
	}
	fn := js[start : start+end]
	if !strings.Contains(js, "let _previewGen = 0;") {
		t.Error("_previewGen counter must be declared")
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
	if !strings.Contains(stopFn, "_previewGen++;") {
		t.Error("stopPreviewPolling() must bump _previewGen so selectSession/createSession invalidate in-flight previews")
	}
	if !strings.Contains(fn, "stopPreviewPolling();\n  const gen = _previewGen;") {
		t.Error("previewDiscovered must call stopPreviewPolling() FIRST and then capture gen = _previewGen (capturing before the call would be invalidated by its own bump)")
	}
	if strings.Contains(fn, "++_previewGen") {
		t.Error("previewDiscovered must not bump _previewGen itself — the bump belongs to stopPreviewPolling()")
	}
	if strings.Count(fn, "if (gen !== _previewGen) return;") < 3 {
		t.Error("previewDiscovered must check the generation after the awaited fetch (ok + error paths) AND inside the poll tick")
	}
	idxSet := strings.Index(fn, "previewTimer = setInterval(")
	if idxSet < 0 {
		t.Fatal("previewTimer = setInterval( not found")
	}
	idxGen := strings.Index(fn, "if (gen !== _previewGen) return;\n    const el = document.getElementById('events-scroll');")
	if idxGen < 0 || idxGen > idxSet {
		t.Fatal("generation check must precede the #events-scroll lookup and previewTimer = setInterval(")
	}
	// Between the post-fetch generation check and arming the interval there
	// must be NO stopPreviewPolling() call: it bumps _previewGen and would
	// invalidate this very call (its tick would bail on the first fire). The
	// prologue call already cleared any older generation's interval.
	if strings.Contains(fn[idxGen:idxSet], "stopPreviewPolling();") {
		t.Error("previewDiscovered must not call stopPreviewPolling() between the gen check and setInterval — it would invalidate its own generation")
	}
}

// TestDashboardJS_SidebarCardRemovalInvalidatesHtmlCache pins that no code
// path removes a .session-card from the DOM without also resetting
// _lastSidebarHtml. renderSidebar skips `list.innerHTML = html` when the
// freshly built string equals the cache; a DOM-only card.remove() leaves the
// cache describing a card that is no longer mounted, so a failed DELETE
// (whose .finally re-fetches) could never bring the card back.
func TestDashboardJS_SidebarCardRemovalInvalidatesHtmlCache(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)
	helperIdx := strings.Index(js, "function removeSidebarCard(key) {")
	if helperIdx < 0 {
		t.Fatal("removeSidebarCard(key) helper must exist")
	}
	helper := js[helperIdx:]
	if end := strings.Index(helper, "\n}\n"); end > 0 {
		helper = helper[:end]
	}
	if !strings.Contains(helper, "if (card) card.remove();") || !strings.Contains(helper, "_lastSidebarHtml = null;") {
		t.Error("removeSidebarCard must remove the card AND set _lastSidebarHtml = null")
	}
	// The helper must be the ONLY place a session card is removed directly.
	if n := strings.Count(js, "if (card) card.remove();"); n != 1 {
		t.Errorf("found %d `if (card) card.remove();` sites; only removeSidebarCard() may remove a session card directly (it invalidates _lastSidebarHtml)", n)
	}
	// dismissSession (the optimistic-delete path with the failure re-sync)
	// must use the helper.
	start := strings.Index(js, "async function dismissSession(")
	if start < 0 {
		t.Fatal("dismissSession not found")
	}
	end := strings.Index(js[start:], "async function renameSession(")
	if end < 0 {
		t.Fatal("renameSession must follow dismissSession")
	}
	dismiss := js[start : start+end]
	if strings.Count(dismiss, "removeSidebarCard(key)") < 3 {
		t.Errorf("dismissSession must call removeSidebarCard(key) on every DOM-removal branch (cron guard / discovered / optimistic delete), got %d", strings.Count(dismiss, "removeSidebarCard(key)"))
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
