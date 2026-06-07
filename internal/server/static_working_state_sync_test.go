package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_OptimisticSidebarSync pins the fix for the "对话框已显示
// working 但左边栏卡片还是旧状态" desync. markSessionOptimisticRunning used to
// flip only sessionsData + the main banner; the sidebar card's dot/label
// waited for the server's session_state push (hundreds of ms during a CLI
// spawn) or the 5s sessions poll. The fix patches the sidebar card DOM in the
// same call via patchSidebarCardState so both surfaces flip together.
func TestDashboardJS_OptimisticSidebarSync(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	if !strings.Contains(js, "function patchSidebarCardState(") {
		t.Error("patchSidebarCardState helper must exist — it keeps the sidebar card dot/label in sync with the main banner on the optimistic flip")
	}

	// The optimistic-running flip must drive the sidebar in place. Without this
	// the left list lags behind the conversation banner — the exact desync the
	// user reported.
	markIdx := strings.Index(js, "function markSessionOptimisticRunning(")
	if markIdx < 0 {
		t.Fatal("markSessionOptimisticRunning not found")
	}
	markEnd := strings.Index(js[markIdx:], "\n}\n")
	if markEnd < 0 {
		t.Fatal("could not bound markSessionOptimisticRunning body")
	}
	markBody := js[markIdx : markIdx+markEnd]
	if !strings.Contains(markBody, "patchSidebarCardState(key, node, 'running')") {
		t.Error("markSessionOptimisticRunning must call patchSidebarCardState(...,'running') so the sidebar card flips to running at send time, not after the server push")
	}

	// The rollback path (busy/error/lost push) must also restore the sidebar
	// card, otherwise a rejected send leaves a stuck green dot on the left.
	rollIdx := strings.Index(js, "function rollbackOptimisticRunning(")
	if rollIdx < 0 {
		t.Fatal("rollbackOptimisticRunning not found")
	}
	rollEnd := strings.Index(js[rollIdx:], "\n}\n")
	if rollEnd < 0 {
		t.Fatal("could not bound rollbackOptimisticRunning body")
	}
	rollBody := js[rollIdx : rollIdx+rollEnd]
	if !strings.Contains(rollBody, "patchSidebarCardState(key, node, 'ready')") {
		t.Error("rollbackOptimisticRunning must call patchSidebarCardState(...,'ready') so a rejected/timed-out send doesn't leave a stuck running dot in the sidebar")
	}

	// patchSidebarCardState must mirror the dot-class mapping used by
	// onSessionState so the optimistic and server-confirmed states render
	// identically (no flicker when the real push arrives).
	patchIdx := strings.Index(js, "function patchSidebarCardState(")
	patchBody := js[patchIdx : patchIdx+strings.Index(js[patchIdx:], "\n}\n")]
	if !strings.Contains(patchBody, "dot-running") || !strings.Contains(patchBody, "dot-ready") || !strings.Contains(patchBody, "dot-new") {
		t.Error("patchSidebarCardState must map state→dot-running/dot-ready/dot-new identically to onSessionState to avoid a flip-flicker when the server state arrives")
	}
}

// TestDashboardJS_JustSentBannerLabel pins the distinct "已发送，正在处理…"
// banner line shown during the CLI-spawn window. Before the first real turn
// event arrives the banner used a generic static "处理中…" that read as "no
// response"; justSent gives an explicit "received, starting up" signal that is
// cleared by the first real event.
func TestDashboardJS_JustSentBannerLabel(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	if !strings.Contains(js, "turnState.justSent = true;") {
		t.Error("markSessionOptimisticRunning must set turnState.justSent so the banner shows the distinct send-acknowledged label")
	}
	if !strings.Contains(js, "turnState.justSent = false;") {
		t.Error("applyEventToTurnState must clear turnState.justSent once a real event arrives so the normal activity labels take over")
	}
	if !strings.Contains(js, "actEl.textContent = '已发送，正在处理…';") {
		t.Error("refreshBanner must render '已发送，正在处理…' while justSent is set — the generic '处理中…' read as no-response during CLI spawn")
	}

	// Ordering guard: justSent must be checked AFTER the real-activity branches
	// (currentTool/isThinking/isWriting) so a genuine tool/thinking event wins
	// over the transient send-ack label.
	bannerIdx := strings.Index(js, "// Line 1: current activity")
	if bannerIdx < 0 {
		t.Fatal("refreshBanner line-1 block not found")
	}
	blockEnd := bannerIdx + 1200
	if blockEnd > len(js) {
		blockEnd = len(js)
	}
	block := js[bannerIdx:blockEnd]
	thinkIdx := strings.Index(block, "actEl.textContent = '思考中...';")
	justSentIdx := strings.Index(block, "actEl.textContent = '已发送，正在处理…';")
	if thinkIdx < 0 || justSentIdx < 0 || justSentIdx < thinkIdx {
		t.Error("the justSent label must be ordered after the thinking/tool/writing branches so real activity overrides the transient send-ack text")
	}
}

// TestDashboardHTML_BannerVisualEmphasis pins the visual cues that make the
// running banner register immediately as "working": a fade/slide-in on
// appearance and a ripple ring on the status dot. Without these the 13px bar
// was easy to miss, contributing to the "感觉没反应" report.
func TestDashboardHTML_BannerVisualEmphasis(t *testing.T) {
	t.Parallel()
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	css := string(data)

	if !strings.Contains(css, "@keyframes rb-appear") {
		t.Error("dashboard.html must define @keyframes rb-appear — the banner fade/slide-in draws the eye when work starts")
	}
	if !strings.Contains(css, "animation:rb-appear") {
		t.Error(".running-banner must use the rb-appear animation so it visibly enters rather than popping in unnoticed")
	}
	if !strings.Contains(css, "@keyframes rb-dot-ripple") {
		t.Error("dashboard.html must define @keyframes rb-dot-ripple — the expanding ring makes the status dot read as actively working")
	}
	if !strings.Contains(css, "rb-dot-ripple") || !strings.Contains(css, "animation:pulse 1.5s ease-in-out infinite,rb-dot-ripple") {
		t.Error(".running-dot must layer rb-dot-ripple on top of the existing pulse so the working indicator is unmistakable")
	}
}

// TestDashboardJS_PollReconcilesDroppedRunningPush pins the self-heal for the
// one left/right desync the push-only rule leaves open: when a 'running'
// session_state WS push is DROPPED, renderSidebar still paints the selected
// card as running from the REST snapshot, but the right-side banner — which is
// only ever flipped to running by the push — stays idle. The poll reconcile
// must recover the banner, but ONLY when it is fully hidden, so a banner that
// is correctly showing live activity (or zero-downtime background agents) is
// never flickered by a lagging REST snapshot.
func TestDashboardJS_PollReconcilesDroppedRunningPush(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// Bound the fetchSessions reconcile block so the assertions can't match a
	// similarly-shaped condition elsewhere.
	anchor := strings.Index(js, "// Reconcile main area state:")
	if anchor < 0 {
		t.Fatal("fetchSessions reconcile block not found")
	}
	end := anchor + 2600
	if end > len(js) {
		end = len(js)
	}
	block := js[anchor:end]

	// The recovery branch must exist and gate on a live WS + running REST state
	// (the exact condition the finished-direction branch refuses to handle).
	if !strings.Contains(block, "wsConnected && sd.state === 'running'") {
		t.Error("poll reconcile must add a branch for (WS connected && REST says running) to recover a dropped running push")
	}
	// It must reconcile toward running only when the banner is hidden — an
	// already-visible banner must be left untouched to avoid flicker and to
	// preserve the zero-downtime background-agent banner.
	if !strings.Contains(block, "banner.style.display === 'none'") {
		t.Error("the dropped-push recovery must fire only when the running-banner is hidden, so a correctly-showing banner is never flickered")
	}
	if !strings.Contains(block, "updateMainState('running'") {
		t.Error("the dropped-push recovery must call updateMainState('running', ...) to bring the right-side banner in sync with the sidebar")
	}

	// Guard the invariant the recovery must NOT break: the finished-direction
	// branch still refuses to reconcile toward running over a live WS in the
	// general case (only the hidden-banner recovery is the exception).
	if !strings.Contains(block, "sd.state !== 'running' && !sessionOptimisticRunning[sKey]") {
		t.Error("the original finished-direction reconcile (state!=='running' && !optimistic) must remain — the recovery is an additive exception, not a replacement")
	}
}
