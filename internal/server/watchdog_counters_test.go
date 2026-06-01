package server

import "testing"

// TestWatchdogCountersPtrsAlias verifies that noOutPtr/totalPtr return live
// handles onto the grouped counters: mutating through the returned pointer is
// observed by Load() and the two counters stay independent. This guards the
// R243-ARCH-7 / #838 regrouping so the by-pointer DI into HealthHandler /
// dispatch keeps sharing the same underlying atomics.
func TestWatchdogCountersPtrsAlias(t *testing.T) {
	var w watchdogCounters

	if got := w.noOutPtr().Load(); got != 0 {
		t.Fatalf("fresh noOutput = %d, want 0", got)
	}
	if got := w.totalPtr().Load(); got != 0 {
		t.Fatalf("fresh total = %d, want 0", got)
	}

	w.noOutPtr().Add(2)
	w.totalPtr().Add(5)

	if got := w.noOutput.Load(); got != 2 {
		t.Errorf("noOutput after Add(2) = %d, want 2", got)
	}
	if got := w.total.Load(); got != 5 {
		t.Errorf("total after Add(5) = %d, want 5", got)
	}

	// Independent counters: bumping one must not move the other.
	w.noOutPtr().Add(1)
	if got := w.total.Load(); got != 5 {
		t.Errorf("total leaked after noOutput bump = %d, want 5", got)
	}

	// Returned pointers are stable across calls (same backing field).
	if w.noOutPtr() != w.noOutPtr() {
		t.Error("noOutPtr returned different addresses across calls")
	}
}
