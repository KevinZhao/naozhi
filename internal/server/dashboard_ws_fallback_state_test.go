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
		t.Fatalf("%q not found in the dashboard modules", marker)
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

// TestDashboardJS_WSSetStateRespectsHiddenTab pins #2431 P3: wsStateChanged must
// not (re)arm the fallback pollers while document.hidden — stopPollers already
// suspended them and startPollers re-arms on visibilitychange.
func TestDashboardJS_WSSetStateRespectsHiddenTab(t *testing.T) {
	t.Parallel()
	body := jsBlockBody(t, readDashboardJS(t), "function wsStateChanged(s, prev) {")

	if !strings.Contains(body, "document.hidden") {
		t.Fatal("wsStateChanged has no document.hidden gate — a WS transition on a hidden tab re-arms pollers stopPollers just suspended")
	}
	// Every fallback interval armed in setState must sit behind the gate.
	for _, arm := range []string{
		"timers.sessionPoll = setInterval(fetchSessions, 5000);",
		"timers.discoveredPoll = setInterval(scanDiscovered, 5000);",
		"timers.discoveredPoll = setInterval(scanDiscovered, 30000);",
		"timers.events = setInterval(() => fetchEvents(false), 1000);",
	} {
		i := strings.Index(body, arm)
		if i < 0 {
			t.Fatalf("expected %q in wsStateChanged", arm)
		}
		lineStart := strings.LastIndex(body[:i], "\n") + 1
		line := body[lineStart:i]
		if !strings.Contains(line, "visible") && !strings.Contains(line, "!document.hidden") {
			t.Errorf("%q is armed unconditionally in wsStateChanged (line=%q); must be gated on tab visibility", arm, strings.TrimSpace(line))
		}
	}
}

// TestDashboardJS_StartPollersSkipsSessionsPollWhenWSConnected pins #2431 P3:
// the visibilitychange resume path must not arm the 5 s sessions poll when the
// WS is live — otherwise REST polling and WS pushes run side by side until the
// next reconnect.
func TestDashboardJS_StartPollersSkipsSessionsPollWhenWSConnected(t *testing.T) {
	t.Parallel()
	body := jsBlockBody(t, readDashboardJS(t), "  const startPollers = () => {")

	const arm = "timers.sessionPoll = setInterval(fetchSessions, 5000);"
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

// TestDashboardJS_StartPollersForcesFullReconcileOnResume pins the F2 follow-up
// to #2431: startPollers no longer arms the sessions poll over a CONNECTED
// socket, but a half-open socket still reads CONNECTED and delivers nothing.
// The one-shot resume fetch must bypass the version gate (lastVersion = 0) so
// returning to the tab always repaints from REST at least once.
func TestDashboardJS_StartPollersForcesFullReconcileOnResume(t *testing.T) {
	t.Parallel()
	body := jsBlockBody(t, readDashboardJS(t), "  const startPollers = () => {")
	zi := strings.Index(body, "sessionList.lastVersion = 0;")
	fi := strings.Index(body, "fetchSessions();")
	if zi < 0 || fi < 0 || zi > fi {
		t.Fatalf("startPollers must zero sessionList.lastVersion before the one-shot fetchSessions() (zero=%d, fetch=%d)", zi, fi)
	}
}
