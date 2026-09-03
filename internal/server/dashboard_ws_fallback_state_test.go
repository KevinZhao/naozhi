package server

import (
	"strings"
	"testing"
)

// jsBlockBody returns the source of the first function/method whose declaration
// starts with marker, bounded by the next line that is exactly "}" or "  }," /
// "  }" (top-level function or object-literal method). Fails the test when the
// marker is absent so a rename shows up as a clear message, not a slice panic.
func jsBlockBody(t *testing.T, js, marker string) string {
	t.Helper()
	start := strings.Index(js, marker)
	if start < 0 {
		t.Fatalf("%q not found in dashboard.js", marker)
	}
	rest := js[start:]
	end := -1
	for _, term := range []string{"\n}\n", "\n  },\n", "\n  }\n", "\n  };\n"} {
		if i := strings.Index(rest, term); i >= 0 && (end < 0 || i < end) {
			end = i
		}
	}
	if end < 0 {
		t.Fatalf("could not bound body of %q", marker)
	}
	return rest[:end]
}

// TestDashboardJS_FetchSessionsVersionGateSkippedWhenWSDown pins #2431 P2: the
// stats.version short-circuit in fetchSessions must only apply while the WS is
// connected. Under fallback polling REST is the sole state source and process
// state flips never advance stats.version, so an unconditional early return
// froze the sidebar and made the "WS disconnected → always reconcile" branch
// unreachable.
func TestDashboardJS_FetchSessionsVersionGateSkippedWhenWSDown(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read embedded dashboard.js: %v", err)
	}
	body := jsBlockBody(t, string(data), "async function fetchSessions() {")

	const gate = "version === lastVersion && version > 0"
	gi := strings.Index(body, gate)
	if gi < 0 {
		t.Fatalf("version short-circuit %q missing from fetchSessions", gate)
	}
	// The gate line must be conditioned on wsConnected.
	lineStart := strings.LastIndex(body[:gi], "\n") + 1
	line := body[lineStart : gi+len(gate)]
	if !strings.Contains(line, "wsConnected &&") {
		t.Fatalf("fetchSessions version short-circuit is not gated on wsConnected; line=%q", strings.TrimSpace(line))
	}
	// wsConnected must be derived from the WS manager BEFORE the gate.
	wi := strings.Index(body, "const wsConnected = wsm.state === WS_STATES.CONNECTED;")
	if wi < 0 || wi > gi {
		t.Fatalf("wsConnected must be computed from wsm.state before the version gate (idx=%d, gate=%d)", wi, gi)
	}
	// Exactly one definition — the old duplicate inside the reconcile block
	// shadowing a differently-scoped value is what let the two drift apart.
	if n := strings.Count(body, "const wsConnected ="); n != 1 {
		t.Fatalf("expected exactly 1 wsConnected definition in fetchSessions, got %d", n)
	}
	// The disconnected-reconcile branch must still exist downstream.
	if ri := strings.Index(body, "!wsConnected ||"); ri < 0 || ri < gi {
		t.Fatalf("WS-disconnected reconcile branch missing or ordered before the gate (idx=%d)", ri)
	}
}

// TestDashboardJS_OptimisticRunningWrittenBackToSidebarPayload pins #2431 P3:
// the optimistic 'running' copy created in fetchSessions must be written back
// into the payload handed to renderSidebar / _lastSidebarData, otherwise a
// cached re-render (toggleProjectCollapsed, sidebar search) paints the card
// 'ready' while the main banner says running.
func TestDashboardJS_OptimisticRunningWrittenBackToSidebarPayload(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read embedded dashboard.js: %v", err)
	}
	body := jsBlockBody(t, string(data), "async function fetchSessions() {")

	oi := strings.Index(body, "s = Object.assign({}, s, { state: 'running' });")
	if oi < 0 {
		t.Fatal("optimistic running copy missing from fetchSessions")
	}
	wb := strings.Index(body, "data = Object.assign({}, data, { sessions });")
	if wb < 0 {
		t.Fatal("fetchSessions does not write the mapped sessions back into data (optimistic copy lost for renderSidebar/_lastSidebarData)")
	}
	rs := strings.Index(body, "renderSidebar(data);")
	ls := strings.Index(body, "_lastSidebarData = data;")
	if rs < 0 || ls < 0 {
		t.Fatalf("renderSidebar(data)/_lastSidebarData = data missing (rs=%d, ls=%d)", rs, ls)
	}
	if !(oi < wb && wb < rs && rs < ls) {
		t.Fatalf("order must be copy(%d) < write-back(%d) < renderSidebar(%d) < _lastSidebarData(%d)", oi, wb, rs, ls)
	}
}

// TestDashboardJS_WSSetStateRespectsHiddenTab pins #2431 P3: wsm.setState must
// not (re)arm the fallback pollers while document.hidden — stopPollers already
// suspended them and startPollers re-arms on visibilitychange.
func TestDashboardJS_WSSetStateRespectsHiddenTab(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read embedded dashboard.js: %v", err)
	}
	body := jsBlockBody(t, string(data), "  setState(s) {")

	if !strings.Contains(body, "document.hidden") {
		t.Fatal("wsm.setState has no document.hidden gate — a WS transition on a hidden tab re-arms pollers stopPollers just suspended")
	}
	// Every fallback interval armed in setState must sit behind the gate.
	for _, arm := range []string{
		"sessionPollTimer = setInterval(fetchSessions, 5000);",
		"discoveredPollTimer = setInterval(scanDiscovered, 5000);",
		"discoveredPollTimer = setInterval(scanDiscovered, 30000);",
		"eventTimer = setInterval(() => fetchEvents(false), 1000);",
	} {
		i := strings.Index(body, arm)
		if i < 0 {
			t.Fatalf("expected %q in setState", arm)
		}
		lineStart := strings.LastIndex(body[:i], "\n") + 1
		line := body[lineStart:i]
		if !strings.Contains(line, "visible") && !strings.Contains(line, "!document.hidden") {
			t.Errorf("%q is armed unconditionally in setState (line=%q); must be gated on tab visibility", arm, strings.TrimSpace(line))
		}
	}
}

// TestDashboardJS_StartPollersSkipsSessionsPollWhenWSConnected pins #2431 P3:
// the visibilitychange resume path must not arm the 5 s sessions poll when the
// WS is live — otherwise REST polling and WS pushes run side by side until the
// next reconnect.
func TestDashboardJS_StartPollersSkipsSessionsPollWhenWSConnected(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read embedded dashboard.js: %v", err)
	}
	body := jsBlockBody(t, string(data), "  const startPollers = () => {")

	const arm = "sessionPollTimer = setInterval(fetchSessions, 5000);"
	ai := strings.Index(body, arm)
	if ai < 0 {
		t.Fatalf("expected %q in startPollers", arm)
	}
	const guard = "wsm.state === WS_STATES.CONNECTED"
	gi := strings.Index(body, guard)
	if gi < 0 || gi > ai {
		t.Fatalf("startPollers arms the sessions poll without checking WS state first (guard=%d, arm=%d)", gi, ai)
	}
	// The one-shot refresh on resume must stay — it is what makes the returning
	// tab non-stale regardless of WS state.
	if fi := strings.Index(body, "fetchSessions();"); fi < 0 || fi > ai {
		t.Fatalf("startPollers must still fetchSessions() once before arming (idx=%d)", fi)
	}
}

// TestDashboardJS_FallbackReconcileComparesLastAppliedState pins the F1 follow-
// up to #2431: with the version gate open, the fetchSessions reconcile runs every
// 5 s under fallback, but updateSendButton is not idempotent ('running' re-seeds
// agent rows from the REST snapshot, 'ready' resets turn state / swaps the
// loading indicator / scrolls). The reconcile must therefore compare the REST
// state against the last state the main area actually applied — recorded in
// updateSendButton (the common sink for WS push, optimistic flip, renderMainShell
// and REST reconcile) and cleared on session switch.
func TestDashboardJS_FallbackReconcileComparesLastAppliedState(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read embedded dashboard.js: %v", err)
	}
	js := string(data)

	fetch := jsBlockBody(t, js, "async function fetchSessions() {")
	const apply = "updateMainState(sd.state, sd.death_reason);"
	ai := strings.Index(fetch, apply)
	if ai < 0 {
		t.Fatalf("expected %q in fetchSessions reconcile", apply)
	}
	// The comparison must sit between the reconcile branch head and the apply.
	head := strings.LastIndex(fetch[:ai], "if (sd && (!wsConnected ||")
	if head < 0 {
		t.Fatal("reconcile branch head not found before updateMainState")
	}
	guard := fetch[head:ai]
	if !strings.Contains(guard, "_lastAppliedMainState") || !strings.Contains(guard, "applied.state === sd.state") {
		t.Fatalf("fetchSessions reconcile must compare sd.state with _lastAppliedMainState before updateMainState; got %q", guard)
	}

	// updateSendButton is the single recording point, keyed by the selected
	// session so a stale record from another session can never suppress the
	// first reconcile after a switch.
	usb := jsBlockBody(t, js, "function updateSendButton(state) {")
	if !strings.Contains(usb, "_lastAppliedMainState = { key: sid(selectedKey, selectedNode), state: state };") {
		t.Fatal("updateSendButton must record _lastAppliedMainState = {key, state} for the selected session")
	}

	// selectSession must clear the record so the new session's first poll
	// reconciles unconditionally.
	// selectSession has 2-space-indented inner closers, so bound it by the next
	// top-level function declaration instead of jsBlockBody.
	ss := strings.Index(js, "function selectSession(key, node) {")
	if ss < 0 {
		t.Fatal("selectSession not found")
	}
	se := strings.Index(js[ss+1:], "\nfunction ")
	if se < 0 {
		t.Fatal("could not bound selectSession")
	}
	sel := js[ss : ss+1+se]
	if !strings.Contains(sel, "_lastAppliedMainState = null;") {
		t.Fatal("selectSession must reset _lastAppliedMainState on session switch")
	}
}

// TestDashboardJS_StartPollersForcesFullReconcileOnResume pins the F2 follow-up
// to #2431: startPollers no longer arms the sessions poll over a CONNECTED
// socket, but a half-open socket still reads CONNECTED and delivers nothing.
// The one-shot resume fetch must bypass the version gate (lastVersion = 0) so
// returning to the tab always repaints from REST at least once.
func TestDashboardJS_StartPollersForcesFullReconcileOnResume(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read embedded dashboard.js: %v", err)
	}
	body := jsBlockBody(t, string(data), "  const startPollers = () => {")
	zi := strings.Index(body, "lastVersion = 0;")
	fi := strings.Index(body, "fetchSessions();")
	if zi < 0 || fi < 0 || zi > fi {
		t.Fatalf("startPollers must zero lastVersion before the one-shot fetchSessions() (zero=%d, fetch=%d)", zi, fi)
	}
}
