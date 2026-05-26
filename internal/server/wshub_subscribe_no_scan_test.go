package server

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestHandleSubscribe_NoFullClientScan pins R242-GO-5 (#552) /
// R246-PERF-4 (#716): the per-key subscriber-cap check in handleSubscribe
// must read h.subscriberCount[key] (O(1)) instead of iterating h.clients
// while holding h.mu (O(N_clients) and on the lock-time hot path).
//
// The historical regression was a `for cli := range h.clients` loop that
// summed per-client subscription-map hits to recompute the cap each
// subscribe; under realistic dashboard fan-out (multi-tab × multi-device
// × notification subscriptions) this both ballooned lock-hold time and
// blocked every concurrent send/broadcast through the same h.mu.
//
// Source-level pin (rather than a benchmark) keeps the assertion robust
// to alloc / GC noise and makes the failure mode descriptive: the
// subsequent reviewer sees the literal forbidden pattern, not a flaky
// "this got slower" complaint.
func TestHandleSubscribe_NoFullClientScan(t *testing.T) {
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

	// Locate the handleSubscribe body. The closing `\n}\n` at column 0
	// terminates the function reliably because the file is gofmt-clean.
	startIdx := strings.Index(body, "func (h *Hub) handleSubscribe(")
	if startIdx < 0 {
		t.Fatal("could not locate handleSubscribe in wshub_subscribe.go")
	}
	rest := body[startIdx:]
	endRe := regexp.MustCompile(`(?m)^\}\n`)
	endLoc := endRe.FindStringIndex(rest)
	if endLoc == nil {
		t.Fatal("could not locate end of handleSubscribe body")
	}
	fnBody := rest[:endLoc[1]]

	// Forbidden patterns: any iteration over h.clients would re-introduce
	// the O(N_clients) lock-time path. completeSubscribe / handleUnsubscribe
	// live in separate helpers (anyOtherClientSubscribesLocked) and are
	// off this critical path, so checking only the handleSubscribe body
	// avoids false positives.
	forbidden := []string{
		"for _, other := range h.clients",
		"for other := range h.clients",
		"for _, c := range h.clients",
		"for c := range h.clients",
		"for cli := range h.clients",
	}
	for _, f := range forbidden {
		if strings.Contains(fnBody, f) {
			t.Errorf("handleSubscribe iterates h.clients (%q) — re-introduces R242-GO-5 / "+
				"R246-PERF-4 (#552 / #716) lock-time scan; use h.subscriberCount[key] instead",
				f)
		}
	}

	// Required: the cap check MUST read h.subscriberCount[key]. A future
	// refactor that replaces the counter with a different bookkeeping
	// shape should leave the pin in place by introducing a new constant
	// fragment to grep on, not by deleting this assertion.
	if !strings.Contains(fnBody, "h.subscriberCount[key]") {
		t.Error("handleSubscribe must consult h.subscriberCount[key] for the per-key cap " +
			"check (R246-PERF-4 / #716)")
	}
	// And the same path must increment the counter on a fresh subscribe so
	// the bound stays consistent. The post-install increment is guarded by
	// `!alreadySub` so the counter doesn't double-count an existing-key
	// re-subscribe.
	if !strings.Contains(fnBody, "h.subscriberCount[key]++") {
		t.Error("handleSubscribe must bump h.subscriberCount[key] when installing a new " +
			"subscription so the cap stays accurate (R246-PERF-4 / #716)")
	}
}
