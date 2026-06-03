package cron

import (
	"testing"
	"time"
)

// TestTrimBulkRemoveExceedsInitialCap verifies trimJobLocked correctly removes
// far more than the toRemove slice's initial cap (4) in a single pass, so the
// R20260603-PERF-12 right-sizing (make([]string, 0, 4)) relies only on append
// growth and never under-trims. keepCount is small (5) while we Append 30 runs,
// so 25 runs must be removed in one trim — well past the 4-slot initial cap.
func TestTrimBulkRemoveExceedsInitialCap(t *testing.T) {
	const keepCount = 5
	const total = 30
	s := newTestStore(t, keepCount, time.Hour)
	jobID := mustGenerateID()
	base := time.Now().Add(-time.Hour)
	for i := 0; i < total; i++ {
		s.Append(makeRun(jobID, base.Add(time.Duration(i)*time.Second)))
	}

	// trimJobLocked runs via Append's GC; force a deterministic trim too.
	s.trimJobUnderLock(jobID, time.Now())

	// The load-bearing invariant for the right-sized toRemove slice: a bulk
	// trim removing 25 runs (>> the initial cap of 4) still enforces the
	// keepCount cap exactly. If append growth were mis-handled, more than
	// keepCount rows would survive on disk.
	rows := s.Recent(jobID, total)
	if len(rows) > keepCount {
		t.Fatalf("bulk trim under-removed: got %d rows, want <= %d", len(rows), keepCount)
	}
	if got := countJSONFiles(t, s.root); got > keepCount {
		t.Fatalf("bulk trim left %d run files on disk, want <= %d", got, keepCount)
	}
}
