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

	// Returned pointers are stable across calls and alias the backing field.
	// R112714-GO-1: compare against &w.noOutput / &w.total (different
	// expressions) rather than calling noOutPtr() twice (SA4000 flags
	// self-comparison as always-false).
	if w.noOutPtr() != &w.noOutput {
		t.Error("noOutPtr does not alias &w.noOutput")
	}
	if w.totalPtr() != &w.total {
		t.Error("totalPtr does not alias &w.total")
	}
}

// TestWatchdogCountersSnapshot verifies the unified read-side view matches what
// the individual loads report, so handlers can migrate off open-coded
// .Load() pairs (#838) without changing observed values.
func TestWatchdogCountersSnapshot(t *testing.T) {
	var w watchdogCounters

	if snap := w.Snapshot(); snap != (watchdogSnapshot{}) {
		t.Fatalf("fresh Snapshot = %+v, want zero", snap)
	}

	w.noOutPtr().Add(3)
	w.totalPtr().Add(7)

	snap := w.Snapshot()
	if snap.NoOutputKills != 3 {
		t.Errorf("snap.NoOutputKills = %d, want 3", snap.NoOutputKills)
	}
	if snap.TotalKills != 7 {
		t.Errorf("snap.TotalKills = %d, want 7", snap.TotalKills)
	}
	// Snapshot must agree with the per-field loads it replaces.
	if snap.NoOutputKills != w.noOutPtr().Load() || snap.TotalKills != w.totalPtr().Load() {
		t.Error("Snapshot disagrees with individual counter loads")
	}
}
