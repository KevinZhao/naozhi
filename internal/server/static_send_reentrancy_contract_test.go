package server

import (
	"strings"
	"testing"
)

// sendMessageBody returns the source of the dashboard's sendMessage()
// function so the assertions below cannot be satisfied by a stray match in an
// unrelated composer (the scratch drawer has its own `sending` flag).
func sendMessageBody(t *testing.T) string {
	t.Helper()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)
	start := strings.Index(js, "async function sendMessage() {")
	if start < 0 {
		t.Fatal("sendMessage not found")
	}
	end := strings.Index(js[start:], "\nfunction renderOptimisticUserMsg(")
	if end < 0 {
		t.Fatal("could not bound sendMessage body")
	}
	return js[start : start+end]
}

// TestDashboardJS_SendReentrancyGateBeforeOrientWait pins the #2405 fix
// ("dashboard 首条用户消息被重复发送 4 次"):
//
// sendMessage() awaits awaitPendingOrients() (up to ORIENT_MAX_WAIT_MS) when an
// attached image is still auto-orienting. Its only reentrancy gate is
// `if (sending) return;` at the top, so `sending = true` MUST be set before
// that await — otherwise every Enter pressed during the wait spawns another
// waiter that captures the same text, and once orient settles waiter #1 sends
// text+file_ids while #2..N each fire a text-only ghost over the WS path.
func TestDashboardJS_SendReentrancyGateBeforeOrientWait(t *testing.T) {
	t.Parallel()
	body := sendMessageBody(t)

	gateIdx := strings.Index(body, "await awaitPendingOrients();")
	if gateIdx < 0 {
		t.Fatal("sendMessage must await awaitPendingOrients()")
	}
	// The takeover branch has its own `sending = true`; the one we care about
	// sits on the main path, i.e. after the main-path selectedKey guard.
	mainIdx := strings.Index(body, "if (!selectedKey) return;")
	if mainIdx < 0 || mainIdx > gateIdx {
		t.Fatal("main-path `if (!selectedKey) return;` guard must precede the orient await")
	}
	setIdx := strings.LastIndex(body[:gateIdx], "sending = true;")
	if setIdx < mainIdx {
		t.Error("`sending = true;` must be set on the main path BEFORE `await awaitPendingOrients();` — a send entered during the wait must hit the top-of-function gate (#2405)")
	}
	// The button feedback must move with it (the visible cue that stops the
	// operator from mashing Enter).
	btnIdx := strings.LastIndex(body[:gateIdx], "btn.classList.add('sending');")
	if btnIdx < mainIdx {
		t.Error("#btn-send .sending class must be added before the orient await, alongside sending = true")
	}
	// The pre-fix layout must not come back.
	if strings.Contains(body, "filter(Boolean);\n\n  sending = true;") {
		t.Error("sending = true must not be deferred until after fileIDs are collected (that is the #2405 window)")
	}

	// Once the gate closes before an await, every exit path must reopen it.
	// A single outer try/finally is the only shape that guarantees that.
	if !strings.Contains(body, "} finally {\n    sending = false;\n    if (btn) btn.classList.remove('sending');\n  }\n}") {
		t.Error("sendMessage must reset `sending` in an outer finally that closes the function — early returns after the gate would otherwise wedge the composer")
	}
	// The gate-protected region must not sprinkle ad-hoc resets that would let
	// a later refactor drop one on a new early-return path.
	region := body[setIdx:]
	if n := strings.Count(region, "sending = false;"); n != 1 {
		t.Errorf("expected exactly one `sending = false;` (the outer finally) after the gate closes, got %d", n)
	}
}

// TestDashboardJS_SendRevalidatesAfterOrientWait pins the second half of the
// #2405 fix: the composer stays editable during the orient wait, so text and
// attachments captured before the await can be stale when it returns. The
// synchronous checks must run again afterwards — in particular the
// "uploading" gate: an id-less in-flight upload dropped mid-wait used to be
// filtered out of fileIDs and then deleted by clearPendingFiles().
func TestDashboardJS_SendRevalidatesAfterOrientWait(t *testing.T) {
	t.Parallel()
	body := sendMessageBody(t)
	gateIdx := strings.Index(body, "await awaitPendingOrients();")
	if gateIdx < 0 {
		t.Fatal("sendMessage must await awaitPendingOrients()")
	}
	const validate = "validateComposerForSend("
	if strings.Count(body, validate) != 2 {
		t.Fatalf("sendMessage must call %s exactly twice (before and after the orient await), got %d", validate, strings.Count(body, validate))
	}
	if strings.LastIndex(body, validate) < gateIdx {
		t.Error("the second validateComposerForSend call must come AFTER the orient await")
	}
	// Text must be re-read from the live composer after the await.
	if strings.LastIndex(body, "text = getMsgValue(input);") < gateIdx {
		t.Error("sendMessage must re-read the composer text after the orient await")
	}
	// A session switch during the wait must abort, not redirect the text.
	if !strings.Contains(body[gateIdx:], "selectedKey !== targetKey") {
		t.Error("sendMessage must abort after the orient await when selectedKey changed — the captured text belongs to the previous session")
	}

	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)
	vIdx := strings.Index(js, "function validateComposerForSend(")
	if vIdx < 0 {
		t.Fatal("validateComposerForSend helper missing")
	}
	vBody := js[vIdx : vIdx+strings.Index(js[vIdx:], "\n}\n")]
	for _, want := range []string{
		"f.status === 'uploading'",
		"/^\\s*\\/urgent\\b/",
		"featureForCurrent('embedded_context')",
		"1024 * 1024",
	} {
		if !strings.Contains(vBody, want) {
			t.Errorf("validateComposerForSend must own the synchronous pre-send check %q so both call sites stay in lockstep", want)
		}
	}
}

// TestDashboardJS_OptimisticRunningForUnlistedSession pins the UX half of
// #2405: markSessionOptimisticRunning used to `return` when the session had
// no sessionsData entry yet. A brand-new session's first send therefore got
// no send→stop swap and no "已发送，正在处理…" banner — the missing feedback
// that invited the Enter mashing. The button/banner flip must not depend on
// the server having listed the session.
func TestDashboardJS_OptimisticRunningForUnlistedSession(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	markIdx := strings.Index(js, "function markSessionOptimisticRunning(")
	if markIdx < 0 {
		t.Fatal("markSessionOptimisticRunning not found")
	}
	markBody := js[markIdx : markIdx+strings.Index(js[markIdx:], "\n}\n")]
	if strings.Contains(markBody, "if (!sd) return;") {
		t.Error("markSessionOptimisticRunning must not bail on a missing sessionsData entry — a new session's first send needs the button/banner flip too")
	}
	if !strings.Contains(markBody, "if (sd && sd.state === 'running') return;") {
		t.Error("markSessionOptimisticRunning must only short-circuit when the server already reports running")
	}
	if !strings.Contains(markBody, "updateSendButton('running');") || !strings.Contains(markBody, "turnState.justSent = true;") {
		t.Error("markSessionOptimisticRunning must flip the button and set justSent for the selected session")
	}

	// The rollback must undo the flip even when there is still no
	// sessionsData entry (busy/error ack, 20s watchdog).
	rollIdx := strings.Index(js, "function rollbackOptimisticRunning(")
	if rollIdx < 0 {
		t.Fatal("rollbackOptimisticRunning not found")
	}
	rollBody := js[rollIdx : rollIdx+strings.Index(js[rollIdx:], "\n}\n")]
	if !strings.Contains(rollBody, "if (key === selectedKey && (node || 'local') === selectedNode) updateSendButton('ready');") {
		t.Error("rollbackOptimisticRunning must restore the send button for the selected session regardless of whether sessionsData has the entry")
	}
}

// TestDashboardJS_WSSendConsumesPendingMapsAfterSuccess pins that the WS send
// branch only forgets sessionWorkspaces/sessionNodes/sessionBackends/
// sessionAccessProfiles AFTER wsm.send() reported success. A failed WS send
// falls through to the HTTP path, which must still see those one-shot spawn
// parameters — deleting them while building the frame silently dropped the
// workspace/backend/access profile on that fallback.
func TestDashboardJS_WSSendConsumesPendingMapsAfterSuccess(t *testing.T) {
	t.Parallel()
	body := sendMessageBody(t)
	wsStart := strings.Index(body, "if (wsm.isConnected() && fileIDs.length === 0) {")
	if wsStart < 0 {
		t.Fatal("WS send branch not found")
	}
	httpStart := strings.Index(body, "// HTTP POST fallback")
	if httpStart < 0 || httpStart < wsStart {
		t.Fatal("HTTP fallback comment marker must follow the WS branch")
	}
	ws := body[wsStart:httpStart]
	sendOK := strings.Index(ws, "if (wsm.send(sendMsg)) {")
	if sendOK < 0 {
		t.Fatal("WS branch must gate on wsm.send(sendMsg) success")
	}
	for _, del := range []string{
		"delete sessionWorkspaces[selectedKey];",
		"delete sessionNodes[selectedKey];",
		"delete sessionBackends[selectedKey];",
		"delete sessionAccessProfiles[selectedKey];",
	} {
		idx := strings.Index(ws, del)
		if idx < 0 {
			t.Errorf("WS branch must still consume %s on success", del)
			continue
		}
		if idx < sendOK {
			t.Errorf("%s must run only after wsm.send(sendMsg) succeeded — a failed WS send falls back to HTTP and needs the entry", del)
		}
	}
}
