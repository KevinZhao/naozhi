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
	js := readDashboardJS(t)

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
		"wsm.subscribedKey = null",
		"wsm.subscribedNode = null",
		"wsm.lastEventTimeWs = 0",
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
	js := readDashboardJS(t)

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
	stashIdx := strings.Index(markBody, "perSession.optimisticPrevState[sKey] = sd.state")
	flipIdx := strings.Index(markBody, "sd.state = 'running'")
	if stashIdx < 0 {
		t.Fatal("markSessionOptimisticRunning must record the pre-flip state in perSession.optimisticPrevState — wasDead cannot otherwise tell a real 'running' from the optimistic flip")
	}
	if flipIdx >= 0 && stashIdx > flipIdx {
		t.Error("perSession.optimisticPrevState must be stashed BEFORE `sd.state = 'running'` — stashing after records the flip itself")
	}

	body := extractJSBlock(t, js, "onSessionState(msg) {")

	captureIdx := strings.Index(body, "const optimisticPrevState = perSession.optimisticPrevState[sKey]")
	if captureIdx < 0 {
		t.Fatal("onSessionState must read perSession.optimisticPrevState before deleting it")
	}
	deleteIdx := strings.Index(body, "delete perSession.optimisticPrevState[sKey]")
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

// TestServerMsg_InitialFlagOnlyOnOpeningFrames is the server half of the
// contract test/e2e/transcript_cursors.test.js pins on the client: the
// dashboard treats Initial as authoritative, so an opening frame that
// forgets the flag leaves the pane stuck on the loading placeholder, and a
// backfill frame that wrongly sets it full-page-replaces a live conversation.
// Both failure modes are invisible in unit tests of either side alone, so pin
// the emitters by source inspection.
func TestServerMsg_InitialFlagOnlyOnOpeningFrames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		file string
		// wantInitial: every wsproto.NewHistory construction in this file is an
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
		found := 0
		text := string(src)
		for idx := strings.Index(text, "wsproto.NewHistory("); idx >= 0; idx = strings.Index(text, "wsproto.NewHistory(") {
			// Scan the balanced construction block (frames span lines).
			depth, end := 0, idx
			for ; end < len(text); end++ {
				switch text[end] {
				case '(':
					depth++
				case ')':
					depth--
				}
				if depth == 0 && text[end] == ')' {
					break
				}
			}
			block := text[idx:end]
			found++
			has := strings.Contains(block, "Initial: true")
			if has != tc.wantInitial {
				t.Errorf("%s: history frame Initial=%v, want %v\n  %s",
					tc.file, has, tc.wantInitial, strings.TrimSpace(block))
			}
			text = text[end:]
		}
		// Without this the loop is vacuously green if the emitter is renamed
		// or moved — the test would then pin nothing. Mirrors
		// internal/node/initial_frame_contract_test.go.
		if found == 0 {
			t.Errorf("%s: expected at least one wsproto.NewHistory frame to pin, found none — did the emitter move?", tc.file)
		}
	}
}

// TestDashboardJS_SubscribedAckKeepsNodeForNonPendingTab pins the follow-up to
// #2421 review F1. After the relay rebuilds a dropped remote subscription (or
// reconnects), the remote's `subscribed` ack is fanned out to EVERY local tab
// on the key — the relay injects "node" into each forwarded frame and
// ReverseConn sets Node explicitly. A tab that was not pending must therefore
// take the node from the frame, not fall back to 'local': with subscribedNode
// rewritten to 'local', the subscription_timeout handler's node-match guard
// fails and the bookkeeping is never cleared, so the tab never re-subscribes —
// the exact bug the rebuild fixes.
//
// Invariant before: non-pending `subscribed` → subscribedNode = 'local'
// (drops the node of a remote key).
// Invariant after:  subscribedNode = pending node, else the frame's node,
// else 'local' — a remote key never collapses to 'local'.
func TestDashboardJS_SubscribedAckKeepsNodeForNonPendingTab(t *testing.T) {
	t.Parallel()
	js := readDashboardJS(t)

	body := wsOnHandler(t, js, "subscribed", false)
	if !strings.Contains(body, "wsm.subscribedNode = wsm._pendingSubscribeNode || msg.node || 'local'") {
		t.Error("subscribed handler must resolve subscribedNode as `_pendingSubscribeNode || msg.node || 'local'` — a fanned-out remote ack must not rewrite a remote key's node to 'local'")
	}
	if strings.Contains(body, "wsm.subscribedNode = wsm._pendingSubscribeNode || 'local'") {
		t.Error("subscribed handler must not fall straight back to 'local' when not pending — see rationale above")
	}
}
