package server

import (
	"os"
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
// was dead code. The fix records the pre-flip state and judges on that.
func TestDashboardJS_WasDeadNotMaskedByOptimisticRunning(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	// markSessionOptimisticRunning must stash the real state before overwriting
	// it — nothing downstream can reconstruct it afterwards.
	markIdx := strings.Index(js, "function markSessionOptimisticRunning(")
	if markIdx < 0 {
		t.Fatal("markSessionOptimisticRunning not found")
	}
	markBody := js[markIdx:]
	if end := strings.Index(markBody, "\n}\n"); end > 0 {
		markBody = markBody[:end]
	}
	stashIdx := strings.Index(markBody, "sessionOptimisticPrevState[sKey] = sd.state")
	flipIdx := strings.Index(markBody, "sd.state = 'running'")
	if stashIdx < 0 {
		t.Fatal("markSessionOptimisticRunning must record the pre-flip state in sessionOptimisticPrevState — wasDead cannot otherwise tell a real 'running' from the optimistic flip")
	}
	if flipIdx >= 0 && stashIdx > flipIdx {
		t.Error("sessionOptimisticPrevState must be stashed BEFORE `sd.state = 'running'` — stashing after records the flip itself")
	}

	body := extractJSBlock(t, js, "onSessionState(msg) {")

	captureIdx := strings.Index(body, "const optimisticPrevState = sessionOptimisticPrevState[sKey]")
	if captureIdx < 0 {
		t.Fatal("onSessionState must read sessionOptimisticPrevState before deleting it")
	}
	deleteIdx := strings.Index(body, "delete sessionOptimisticPrevState[sKey]")
	if deleteIdx >= 0 && deleteIdx < captureIdx {
		t.Error("optimisticPrevState must be captured BEFORE its delete — capturing after always reads undefined")
	}
	if !strings.Contains(body, "wasOptimisticRunning ? optimisticPrevState : prevState") {
		t.Error("wasDead must judge on the pre-flip state when the flip was optimistic")
	}
	// Judging on death_reason instead of state==='dead' regresses a distinct
	// bug: mapSendError stamps no_output_timeout/total_timeout without the
	// process necessarily being reaped, so a lingering death_reason on a ready
	// session made every subsequent ordinary send force a full-page resubscribe,
	// wiping the just-sent optimistic bubble.
	if !strings.Contains(body, "const wasDead = effectivePrevState === 'dead'") {
		t.Error("wasDead must be `effectivePrevState === 'dead'`, not a death_reason test — a stale death_reason on a ready session must not force a resubscribe")
	}
	if strings.Contains(body, "prev.death_reason && ") {
		t.Error("wasDead must not gate on prev.death_reason — see comment above; death_reason outlives the death it described")
	}
}

// TestDashboardJS_OnHistoryKeysInitialRenderOnServerFlag pins the fix for "运行中
// 重复点击 session 后消息顺序错乱/丢失" (stale-frame race).
//
// Re-clicking the currently-selected running session calls wsm.subscribe()
// (selectSession skips unsubscribe when the key is unchanged) which sets
// _initialSubscribe=true. An in-flight incremental history frame from the
// superseded subscription could then be consumed AS the initial frame:
// full-page-replacing the pane with a couple of newest events and pushing
// lastRenderedEventTime to the tip — after which the REAL initial frame was
// batch-dropped by the incremental time guard.
//
// The decision MUST key on the server's per-frame Initial flag, not on arrival
// order and not on whether the 'subscribed' ack has landed:
//   - ReverseConn.Subscribe's first-subscriber path emits history from a local
//     goroutine while the ack round-trips through the remote, so history can
//     legitimately arrive first (an ack-gated gate blanks remote sessions).
//   - A superseded eventPushLoop's backfill can land either side of the new ack,
//     so an ack gate does not even close the race it targets.
func TestDashboardJS_OnHistoryKeysInitialRenderOnServerFlag(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	body := extractJSBlock(t, js, "onHistory(msg) {")

	if !strings.Contains(body, "const isInitial = this._initialSubscribe && msg.initial === true") {
		t.Error("onHistory must gate the full-page render on the server's msg.initial flag: `this._initialSubscribe && msg.initial === true`")
	}
	// The flag must only be consumed when an initial frame was actually
	// rendered. Unconditional consumption is what let a backfill frame burn it.
	if !strings.Contains(body, "if (isInitial) this._initialSubscribe = false") {
		t.Error("_initialSubscribe must be consumed only when isInitial holds — an incremental backfill frame must leave it armed for the real initial frame")
	}
	// Guard against a relapse to the ack-ordering assumption.
	if strings.Contains(body, "_pendingSubscribeKey === msg.key) return") {
		t.Error("onHistory must not drop frames based on the 'subscribed' ack having landed — reverseconn's first-subscribe path does not guarantee that order")
	}
}

// TestServerMsg_InitialFlagOnlyOnOpeningFrames is the server half of the
// contract TestDashboardJS_OnHistoryKeysInitialRenderOnServerFlag depends on.
// Since the client now treats Initial as authoritative, an opening frame that
// forgets the flag leaves the pane stuck on the loading placeholder, and a
// backfill frame that wrongly sets it full-page-replaces a live conversation.
// Both failure modes are invisible in unit tests of either side alone, so pin
// the emitters by source inspection.
func TestServerMsg_InitialFlagOnlyOnOpeningFrames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		file string
		// wantInitial: every `Type: "history"` construction in this file is an
		// opening frame and must carry Initial: true. Otherwise none may.
		wantInitial bool
	}{
		// completeSubscribe's three arms: suspended-session history, the
		// pooled/fallback initial page, and the empty frame for running sessions.
		{file: "wshub_subscribe.go", wantInitial: true},
		// eventPushLoop backfill — incremental by construction.
		{file: "wshub_eventpush.go", wantInitial: false},
	} {
		src, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, `Type: "history"`) {
				continue
			}
			has := strings.Contains(line, "Initial: true")
			if has != tc.wantInitial {
				t.Errorf("%s:%d: history frame Initial=%v, want %v\n  %s",
					tc.file, i+1, has, tc.wantInitial, strings.TrimSpace(line))
			}
		}
	}
}
