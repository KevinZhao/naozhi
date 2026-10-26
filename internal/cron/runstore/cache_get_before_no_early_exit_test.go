package runstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCacheGetBefore_OutOfOrderRingNoBreak guards the second, independent
// reason the #2323 break is unsound: the ring is ordered by Append/completion
// order (≈ EndedAt), NOT strictly by StartedAt. A long-running job can complete
// — and thus be pushed to the ring head — after a job that started later but
// finished sooner, so StartedAt is not guaranteed monotonic along the ring.
// diskListNewestFirst deliberately uses `continue` (not break) on the StartedAt
// filter for exactly this reason; cacheGetBefore must mirror it. Here a row
// that passes the cutoff sits BEFORE (newer in ring order) a row that fails it,
// so any StartedAt-based break would drop a qualifying row.
func TestCacheGetBefore_OutOfOrderRingNoBreak(t *testing.T) {
	t.Parallel()

	const keepCount = 50
	s := newTestStore(t, keepCount, 30*24*time.Hour)
	jobID := mustGenerateID()

	now := time.Now()
	cutoff := now.Add(-1 * time.Hour)

	// Append in completion order, which is the ring order (head = last pushed).
	// Mix qualifying (old, < cutoff) and non-qualifying (recent, >= cutoff)
	// rows so neither a prefix nor a suffix is purely one class:
	//   pushed #1 (tail): old      -> qualifies
	//   pushed #2:        recent   -> skipped
	//   pushed #3:        old      -> qualifies
	//   pushed #4 (head): recent   -> skipped
	s.Append(makeRun(jobID, now.Add(-3*time.Hour)))   // old
	s.Append(makeRun(jobID, now.Add(-5*time.Minute))) // recent
	s.Append(makeRun(jobID, now.Add(-2*time.Hour)))   // old
	s.Append(makeRun(jobID, now.Add(-1*time.Minute))) // recent

	if got := s.List(jobID, keepCount, time.Time{}); len(got) != 4 {
		t.Fatalf("warm List len=%d want 4", len(got))
	}

	dir := filepath.Join(s.Dir(), jobID)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	// Both old rows qualify; the recent rows interleaved between them must be
	// skipped via `continue`, and the second old row (deeper in the ring) must
	// still be collected. A StartedAt break would stop at the first old row.
	got, ok := s.cacheGetBefore(jobID, 10, cutoff)
	if !ok {
		t.Fatalf("cacheGetBefore ok=false want true (warm, under cap)")
	}
	if len(got) != 2 {
		t.Fatalf("cacheGetBefore out-of-order ring len=%d want 2 (continue not break); got %+v", len(got), got)
	}
	for _, sm := range got {
		if !sm.StartedAt.Before(cutoff) {
			t.Fatalf("returned row StartedAt %v not < cutoff %v", sm.StartedAt, cutoff)
		}
	}
}
