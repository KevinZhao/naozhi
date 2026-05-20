package sysession

import (
	"fmt"
	"testing"
)

func TestRunRing_EmptyState(t *testing.T) {
	t.Parallel()
	r := newRunRing()
	if r.Len() != 0 {
		t.Errorf("empty ring Len = %d, want 0", r.Len())
	}
	if got := r.Snapshot(); len(got) != 0 {
		t.Errorf("empty ring Snapshot len = %d, want 0", len(got))
	}
	if _, ok := r.Latest(); ok {
		t.Error("empty ring Latest should return ok=false")
	}
}

func TestRunRing_BelowCapacityChronological(t *testing.T) {
	t.Parallel()
	r := newRunRing()
	for i := 0; i < 5; i++ {
		r.Append(DaemonRun{RunID: fmt.Sprintf("run-%d", i)})
	}
	if r.Len() != 5 {
		t.Errorf("Len = %d, want 5", r.Len())
	}
	snap := r.Snapshot()
	for i, run := range snap {
		want := fmt.Sprintf("run-%d", i)
		if run.RunID != want {
			t.Errorf("snap[%d].RunID = %q, want %q", i, run.RunID, want)
		}
	}
	last, ok := r.Latest()
	if !ok || last.RunID != "run-4" {
		t.Errorf("Latest = %q (ok=%v), want %q", last.RunID, ok, "run-4")
	}
}

func TestRunRing_WrapsKeepsNewestChronological(t *testing.T) {
	t.Parallel()
	r := newRunRing()
	// Append 1.5x cap so the ring wraps and discards the oldest half.
	total := runRingCap + runRingCap/2
	for i := 0; i < total; i++ {
		r.Append(DaemonRun{RunID: fmt.Sprintf("run-%d", i)})
	}
	if r.Len() != runRingCap {
		t.Errorf("Len = %d, want %d", r.Len(), runRingCap)
	}
	snap := r.Snapshot()
	for i, run := range snap {
		want := fmt.Sprintf("run-%d", total-runRingCap+i)
		if run.RunID != want {
			t.Errorf("snap[%d].RunID = %q, want %q", i, run.RunID, want)
		}
	}
	last, ok := r.Latest()
	wantLast := fmt.Sprintf("run-%d", total-1)
	if !ok || last.RunID != wantLast {
		t.Errorf("Latest = %q (ok=%v), want %q", last.RunID, ok, wantLast)
	}
}
