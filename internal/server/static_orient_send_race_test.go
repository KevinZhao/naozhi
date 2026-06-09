package server

import (
	"strings"
	"testing"
)

// TestDashboardJS_OrientSendRaceGuard pins the frontend fix for the
// auto-orient/send race: maybeAutoOrient runs as a fire-and-forget vision
// side-call (~12s on Haiku) AFTER the upload flips to 'ready'. If the user
// hit send within that window, sendMessage's TakeAll consumed the upload
// before the rotation's in-place Replace landed, so the original sideways
// image went out and the orient Replace silently missed the consumed entry.
//
// dashboard.js has no JS unit runner, so this Go contract guards the wiring
// against a silent refactor that would re-open the race:
//   - the entry carries an `orienting` flag set across the whole call,
//   - send awaits awaitPendingOrients() before consuming file_ids,
//   - the wait is hard-capped (ORIENT_MAX_WAIT_MS) and the orient fetch is
//     abortable so a slow/hung model can never wedge send.
func TestDashboardJS_OrientSendRaceGuard(t *testing.T) {
	t.Parallel()
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		t.Fatalf("read dashboard.js: %v", err)
	}
	js := string(data)

	for _, want := range []string{
		// The bounded wait constant + the gate helper exist.
		"const ORIENT_MAX_WAIT_MS",
		"function awaitPendingOrients()",
		// maybeAutoOrient marks/clears the in-flight flag.
		"entry.orienting = true",
		"entry.orienting = false",
		// The orient fetch is abortable on the timeout.
		"new AbortController()",
		"signal: ctrl.signal",
		// send awaits the gate before consuming uploads.
		"await awaitPendingOrients();",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("dashboard.js missing orient/send race guard: %q", want)
		}
	}

	// The flag must be cleared in a finally block — a thrown or aborted orient
	// call must never leave send permanently gated. Assert the clear sits in a
	// finally by checking the finally keyword precedes the clear.
	finallyIdx := strings.Index(js, "} finally {")
	clearIdx := strings.Index(js, "entry.orienting = false")
	if finallyIdx < 0 || clearIdx < 0 || clearIdx < finallyIdx {
		t.Error("entry.orienting must be cleared inside maybeAutoOrient's finally block")
	}

	// awaitPendingOrients must be awaited BEFORE file_ids are collected for the
	// send payload — otherwise the wait is pointless (the upload is already
	// referenced). Assert ordering by source position.
	gateIdx := strings.Index(js, "await awaitPendingOrients();")
	fileIDsIdx := strings.Index(js, "const fileIDs = pendingFiles.map")
	if gateIdx < 0 || fileIDsIdx < 0 || gateIdx > fileIDsIdx {
		t.Error("await awaitPendingOrients() must precede fileIDs collection in sendMessage")
	}
}
