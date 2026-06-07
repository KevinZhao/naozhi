// static_turn_watchdog_contract_test.go — regression pins for the stuck
// "处理中..." (running) banner fix.
//
// Background: the banner is driven by the session-level running flag, hidden
// only when updateSendButton('ready')→resetTurnState→refreshBanner runs. That
// fires off the 'result' WS event or the 'ready' session_state broadcast. If
// either terminal signal is dropped while the WebSocket stays connected, there
// was no fallback (fetchSessions reconcile was gated on WS being disconnected,
// and the 20s optimistic safety timer is cleared by the 'running' broadcast),
// so the banner stuck on "处理中..." until the operator switched sessions.
//
// These invariants cannot be exercised behaviourally: the e2e mock-server
// rejects WebSocket upgrades (forces polling fallback), so the connected-WS
// dropped-signal path is unreachable from Playwright. Source-contract pins are
// the only available guard. Each positive assertion uses >=2 distinct witnesses
// per the static_ux_contract_test.go policy.
package server

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func readDashboardJSForWatchdog(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	return string(b)
}

// TestDashboardJS_FetchSessionsReconcilesFinishedTurnOverLiveWS pins the
// relaxed reconcile gate: when the WS is connected, fetchSessions must still
// reconcile the *finished* direction (state !== 'running' and not optimistic),
// so a dropped terminal signal can't leave the banner stuck. Guards against a
// regression back to the old `wsm.state !== WS_STATES.CONNECTED`-only gate.
func TestDashboardJS_FetchSessionsReconcilesFinishedTurnOverLiveWS(t *testing.T) {
	js := readDashboardJSForWatchdog(t)

	// Witness 1: the reconcile considers the connected case (a `wsConnected`
	// boolean, not a bare disconnected-only gate).
	if !strings.Contains(js, "const wsConnected = wsm.state === WS_STATES.CONNECTED;") {
		t.Error("reconcile must compute wsConnected so the connected case is handled, not gated out")
	}

	// Witness 2: the reconcile fires when WS is connected AND the snapshot says
	// the turn is finished AND we're not in the optimistic-running window.
	re := regexp.MustCompile(`!wsConnected \|\| \(sd\.state !== 'running' && !sessionOptimisticRunning\[sKey\]\)`)
	if !re.MatchString(js) {
		t.Error("reconcile must run on (!wsConnected) OR (finished && !optimistic) so a dropped terminal signal self-heals")
	}

	// Forbid: the old gate that skipped reconcile entirely whenever WS connected.
	oldGate := regexp.MustCompile(`if \(selectedKey && wsm\.state !== WS_STATES\.CONNECTED\) \{`)
	if oldGate.MatchString(js) {
		t.Error("the old connected-WS reconcile skip must not return — it reintroduces the stuck banner")
	}
}

// TestDashboardJS_TurnWatchdogLifecycle pins the watchdog that supplies the
// missing reconcile tick while WS is connected (the session poll is stopped in
// that mode). It must start when state goes 'running' and stop otherwise.
func TestDashboardJS_TurnWatchdogLifecycle(t *testing.T) {
	js := readDashboardJSForWatchdog(t)

	// Witness 1: a dedicated watchdog timer + start/stop helpers exist.
	for _, sym := range []string{"_turnWatchdogTimer", "function startTurnWatchdog(", "function stopTurnWatchdog("} {
		if !strings.Contains(js, sym) {
			t.Errorf("missing turn watchdog symbol: %s", sym)
		}
	}

	// Witness 2: updateSendButton arms it on 'running' and disarms otherwise.
	runIdx := strings.Index(js, "startTurnWatchdog();")
	stopIdx := strings.Index(js, "stopTurnWatchdog();")
	if runIdx < 0 || stopIdx < 0 {
		t.Fatal("updateSendButton must call startTurnWatchdog on running and stopTurnWatchdog otherwise")
	}

	// The watchdog must reuse the existing debounced fetch (which routes through
	// the relaxed reconcile gate), not invent a parallel banner-hide path.
	if !strings.Contains(js, "debouncedFetchSessions();") {
		t.Error("watchdog must tick debouncedFetchSessions so it reconciles via the authoritative REST snapshot")
	}

	// Witness 3: the watchdog self-heals when selectedKey is cleared without a
	// non-running updateSendButton call (dismissSession nulls selectedKey in
	// three branches). Otherwise the 15s interval leaks for the page lifetime.
	if !regexp.MustCompile(`if \(!selectedKey\) \{ stopTurnWatchdog\(\); return; \}`).MatchString(js) {
		t.Error("watchdog tick must stop itself when selectedKey is null to avoid a perpetual-poll leak after dismissSession")
	}
}

// TestDashboardJS_TurnBoundaryClearsToolTallies pins that the user/result turn
// boundary in applyEventToTurnState clears the tool tallies, keeping hasContent
// from staying truthy after a turn ends (consistency with resetTurnState).
func TestDashboardJS_TurnBoundaryClearsToolTallies(t *testing.T) {
	js := readDashboardJSForWatchdog(t)

	// Witness 1: the applyEventToTurnState user/result branch clears the tool
	// tallies via assignment so hasContent (refreshBanner) can't stay truthy.
	for _, clear := range []string{
		"turnState.toolOrder = [];",
		"turnState.toolCounts = {};",
		"turnState.toolCount = 0;",
	} {
		if !strings.Contains(js, clear) {
			t.Errorf("turn-boundary branch must clear tool tally: %q", clear)
		}
	}

	// Witness 2: resetTurnState (the other turn-reset path) zeroes the same
	// fields via the object literal — the two paths must stay consistent.
	if !regexp.MustCompile(`toolCount: 0,.*\n.*toolCounts: \{\}, toolOrder: \[\]`).MatchString(js) {
		t.Error("resetTurnState must also zero toolCount/toolCounts/toolOrder")
	}
}
