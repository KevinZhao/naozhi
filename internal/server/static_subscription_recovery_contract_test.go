package server

import (
	"strings"
	"testing"
)

// extractJSBlock returns the substring of js starting at the first occurrence
// of marker and ending at the next "\n  },\n" method terminator (wsm object
// method convention). Fails the test when the marker is missing.
func extractJSBlock(t *testing.T, js, marker string) string {
	t.Helper()
	idx := strings.Index(js, marker)
	if idx < 0 {
		t.Fatalf("marker %q not found in dashboard.js", marker)
	}
	end := strings.Index(js[idx:], "\n  },")
	if end < 0 {
		t.Fatalf("could not bound block starting at %q", marker)
	}
	return js[idx : idx+end]
}

// TestDashboardJS_SubscriptionTimeoutClearsClientBookkeeping pins the fix for
// "后端已出结果但 dashboard 不自动更新" (stale-subscription bug, part 1/2).
//
// Chain: TTL recycles the CLI process → server-side resubscribeEvents times
// out after 60s and DROPS the subscription, emitting session_state
// reason="subscription_timeout" (wshub_eventpush.go). The client previously
// ignored that reason for non-cron sessions: wsm.subscribedKey stayed set, so
// on the next running broadcast every needSub branch evaluated false and the
// whole turn streamed to a subscription that no longer existed server-side.
func TestDashboardJS_SubscriptionTimeoutClearsClientBookkeeping(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	body := extractJSBlock(t, js, "onSessionState(msg) {")

	if !strings.Contains(body, "'subscription_timeout'") {
		t.Fatal("onSessionState must handle reason 'subscription_timeout' — without it the client believes a server-dropped subscription is still live and never resubscribes")
	}
	// The handler must actually clear the client-side subscription identity so
	// needSub case 1 (subscribedKey mismatch) fires on the next running
	// broadcast.
	toIdx := strings.Index(body, "'subscription_timeout'")
	tail := body[toIdx:]
	for _, want := range []string{
		"this.subscribedKey = null",
		"this.subscribedNode = null",
		"this.lastEventTimeWs = 0",
	} {
		if !strings.Contains(tail, want) {
			t.Errorf("subscription_timeout handling must include %q so the next running broadcast triggers a fresh subscribe", want)
		}
	}
}

// TestDashboardJS_WasDeadNotMaskedByOptimisticRunning pins part 2/2 of the
// stale-subscription bug: markSessionOptimisticRunning stamps
// sessionsData.state='running' BEFORE the send round-trip, so for every
// dashboard-initiated send the server's real running broadcast observed
// prevState==='running' and the dead→running resubscribe branch (wasDead)
// was dead code. The fix captures the optimistic flag before clearing it and
// lets it punch through the masked state.
func TestDashboardJS_WasDeadNotMaskedByOptimisticRunning(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	body := extractJSBlock(t, js, "onSessionState(msg) {")

	captureIdx := strings.Index(body, "const wasOptimisticRunning = !!sessionOptimisticRunning[sKey]")
	if captureIdx < 0 {
		t.Fatal("onSessionState must capture sessionOptimisticRunning BEFORE deleting it — wasDead needs to know prev.state was an optimistic flip")
	}
	deleteIdx := strings.Index(body, "delete sessionOptimisticRunning[sKey]")
	if deleteIdx >= 0 && deleteIdx < captureIdx {
		t.Error("wasOptimisticRunning must be captured BEFORE `delete sessionOptimisticRunning[sKey]` — capturing after always reads false")
	}
	if !strings.Contains(body, "prevState !== 'running' || wasOptimisticRunning") {
		t.Error("wasDead must treat an optimistically-flipped 'running' as not-really-running: `prev.death_reason && (prevState !== 'running' || wasOptimisticRunning)`")
	}
}

// TestDashboardJS_OnHistoryDropsPreAckStaleFrames pins the fix for "运行中重复
// 点击 session 后消息顺序错乱/丢失" (stale-frame race).
//
// Re-clicking the currently-selected running session calls wsm.subscribe()
// (selectSession skips unsubscribe when the key is unchanged) which sets
// _initialSubscribe=true. An in-flight incremental history frame from the
// superseded subscription could then be consumed AS the initial frame:
// full-page-replacing the pane with a couple of newest events and pushing
// lastRenderedEventTime to the tip — after which the REAL initial frame was
// batch-dropped by the incremental time guard. Server contract: 'subscribed'
// is always sent before the initial history frame (completeSubscribe;
// node relay reverseconn/relay ack likewise), so any same-key history frame
// arriving while the subscribe is still pending is provably stale.
func TestDashboardJS_OnHistoryDropsPreAckStaleFrames(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	body := extractJSBlock(t, js, "onHistory(msg) {")

	gate := "if (this._initialSubscribe && this._pendingSubscribeKey === msg.key) return;"
	gateIdx := strings.Index(body, gate)
	if gateIdx < 0 {
		t.Fatalf("onHistory must drop history frames that arrive before the 'subscribed' ack when expecting an initial frame — gate %q missing", gate)
	}
	// The gate must sit BEFORE _initialSubscribe is consumed, otherwise the
	// stale frame still burns the initial-render flag.
	consumeIdx := strings.Index(body, "this._initialSubscribe = false")
	if consumeIdx < 0 {
		t.Fatal("could not find _initialSubscribe consumption in onHistory")
	}
	if gateIdx > consumeIdx {
		t.Error("stale-frame gate must run BEFORE `this._initialSubscribe = false` — a stale frame must not consume the initial-render flag")
	}
}
