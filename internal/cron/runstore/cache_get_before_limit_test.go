package runstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCacheGetBefore_LimitTruncation pins R20260606-PERF-11: cacheGetBefore
// must stop scanning the ring as soon as `out` reaches `limit`, even when
// there are more entries in the ring that satisfy the before-cutoff filter.
//
// Strategy: warm a cache with 8 entries, all older than the cutoff, then
// request limit=3.  The returned slice must contain exactly 3 entries (the
// 3 newest ones that pass the filter) and no more.
func TestCacheGetBefore_LimitTruncation(t *testing.T) {
	t.Parallel()

	const keepCount = 20
	s := newTestStore(t, keepCount, 30*24*time.Hour)
	jobID := mustGenerateID()

	now := time.Now()
	// Insert 8 entries, all older than the cutoff we'll use.
	for i := 0; i < 8; i++ {
		startedAt := now.Add(-time.Duration(8-i) * time.Hour) // oldest first
		s.Append(makeRun(jobID, startedAt))
	}

	// Warm with a no-cutoff List so cacheGetBefore sees warm=true.
	if got := s.List(jobID, keepCount, time.Time{}); len(got) != 8 {
		t.Fatalf("warm List len=%d want 8", len(got))
	}

	// Remove the on-disk directory so any disk fallback would return empty.
	dir := filepath.Join(s.Dir(), jobID)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	// cutoff = now: all 8 entries have StartedAt < now so all pass the filter.
	// limit=3 must truncate to exactly 3 results (the 3 newest).
	cutoff := now
	got := s.List(jobID, 3, cutoff)
	if len(got) != 3 {
		t.Fatalf("List(limit=3, before=now) len=%d want 3; got %+v", len(got), got)
	}
	// All returned entries must satisfy the cutoff.
	for _, sm := range got {
		if !sm.StartedAt.Before(cutoff) {
			t.Fatalf("entry with StartedAt %v not before cutoff %v", sm.StartedAt, cutoff)
		}
	}
}
