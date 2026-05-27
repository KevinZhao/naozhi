package server

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestHandleUnsubscribe_NoFullClientScan pins R236-PERF-06 (#513): the
// "is anyone else still subscribed to this key" check that gates the
// historyMarshalCache drop must read h.subscriberCount[key] (O(1))
// instead of walking h.clients while h.mu is held.
//
// Pre-fix, handleUnsubscribe called anyOtherClientSubscribesLocked, which
// iterated h.clients × per-client subscription map under h.mu — making
// every dashboard tab close O(N_clients) on the lock-time hot path and
// blocking concurrent broadcast RLocks.
//
// Source-level pin (rather than a benchmark) keeps the assertion robust
// to alloc / GC noise and gives the next reviewer a literal pattern to
// search for instead of a flaky "this got slower" complaint, matching
// the style of TestHandleSubscribe_NoFullClientScan.
func TestHandleUnsubscribe_NoFullClientScan(t *testing.T) {
	t.Parallel()

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(self)

	src, err := os.ReadFile(filepath.Join(dir, "wshub_subscribe.go"))
	if err != nil {
		t.Fatalf("read wshub_subscribe.go: %v", err)
	}
	body := string(src)

	startIdx := strings.Index(body, "func (h *Hub) handleUnsubscribe(")
	if startIdx < 0 {
		t.Fatal("could not locate handleUnsubscribe in wshub_subscribe.go")
	}
	rest := body[startIdx:]
	endRe := regexp.MustCompile(`(?m)^\}\n`)
	endLoc := endRe.FindStringIndex(rest)
	if endLoc == nil {
		t.Fatal("could not locate end of handleUnsubscribe body")
	}
	fnBody := rest[:endLoc[1]]

	forbidden := []string{
		"for _, other := range h.clients",
		"for other := range h.clients",
		"for _, c := range h.clients",
		"for c := range h.clients",
		"for cli := range h.clients",
		// The retired helper itself: re-introducing it would re-introduce
		// the O(N_clients) scan even if the call site looks innocent.
		"anyOtherClientSubscribesLocked",
	}
	for _, f := range forbidden {
		if strings.Contains(fnBody, f) {
			t.Errorf("handleUnsubscribe iterates h.clients (%q) — re-introduces "+
				"R236-PERF-06 (#513) lock-time scan; consult h.subscriberCount[key] "+
				"after decSubscriberCountLocked instead",
				f)
		}
	}

	// And the helper must be gone from the package: any future caller would
	// silently re-introduce the scan even if handleUnsubscribe stays clean.
	if strings.Contains(body, "func (h *Hub) anyOtherClientSubscribesLocked(") {
		t.Error("anyOtherClientSubscribesLocked is the retired O(N_clients) scan " +
			"(R236-PERF-06 / #513). It must not be re-introduced; use " +
			"h.subscriberCount[key] for any 'is anyone else subscribed?' check.")
	}
}
