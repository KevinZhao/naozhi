package cron

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
	dir := filepath.Join(s.root, jobID)
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

// TestCacheGetBefore_LimitEqualsAvailable verifies that when limit equals
// the number of matching entries cacheGetBefore returns all of them without
// under-counting.
func TestCacheGetBefore_LimitEqualsAvailable(t *testing.T) {
	t.Parallel()

	const keepCount = 20
	s := newTestStore(t, keepCount, 30*24*time.Hour)
	jobID := mustGenerateID()

	now := time.Now()
	for i := 0; i < 5; i++ {
		startedAt := now.Add(-time.Duration(5-i) * time.Hour)
		s.Append(makeRun(jobID, startedAt))
	}

	// Warm.
	if got := s.List(jobID, keepCount, time.Time{}); len(got) != 5 {
		t.Fatalf("warm List len=%d want 5", len(got))
	}

	dir := filepath.Join(s.root, jobID)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	// All 5 entries are before cutoff; limit=5 must return all 5.
	cutoff := now
	got := s.List(jobID, 5, cutoff)
	if len(got) != 5 {
		t.Fatalf("List(limit=5, before=now) len=%d want 5", len(got))
	}
}

// TestCacheGetBefore_SomeBeyondCutoff tests the mixed case: some entries are
// newer than cutoff (will be skipped via continue) and some are older (will be
// collected).  limit must still bound the result.
func TestCacheGetBefore_SomeBeyondCutoff(t *testing.T) {
	t.Parallel()

	const keepCount = 20
	s := newTestStore(t, keepCount, 30*24*time.Hour)
	jobID := mustGenerateID()

	now := time.Now()
	// 3 recent (within last hour, won't pass cutoff) + 6 old (will pass).
	for i := 0; i < 3; i++ {
		startedAt := now.Add(-time.Duration(i+1) * time.Minute)
		s.Append(makeRun(jobID, startedAt))
	}
	for i := 0; i < 6; i++ {
		startedAt := now.Add(-time.Duration(i+3) * time.Hour)
		s.Append(makeRun(jobID, startedAt))
	}

	if got := s.List(jobID, keepCount, time.Time{}); len(got) != 9 {
		t.Fatalf("warm List len=%d want 9", len(got))
	}

	dir := filepath.Join(s.root, jobID)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	// cutoff = 1 hour ago; only the 6 old entries qualify.  limit=4 truncates.
	cutoff := now.Add(-1 * time.Hour)
	got := s.List(jobID, 4, cutoff)
	if len(got) != 4 {
		t.Fatalf("List(limit=4, mixed) len=%d want 4; got %+v", len(got), got)
	}
	for _, sm := range got {
		if !sm.StartedAt.Before(cutoff) {
			t.Fatalf("entry StartedAt %v not before cutoff %v", sm.StartedAt, cutoff)
		}
	}
}
