package cron

import (
	"path/filepath"
	"testing"
	"time"
)

// TestRunStore_ListBeforeCutoffServesFromCacheAtCapWhenPageFills covers
// R242-PERF-9 (#672): when count == keepCount but the page fills strictly
// before reaching the oldest cached row, the result is provably the
// newest-first top-`limit` and we may skip the disk scan. Disk-evicted
// rows are by trim contract older than every cached row, so a disk
// fallback would only confirm the same prefix we already collected.
func TestRunStore_ListBeforeCutoffServesFromCacheAtCapWhenPageFills(t *testing.T) {
	t.Parallel()
	// keepCount=5, insert 5 rows so cache hits exactly the cap.
	s := newTestStore(t, 5, 30*24*time.Hour)
	jobID := mustGenerateID()

	now := time.Now()
	// Insert 5 rows at hourly intervals, newest first will be `now`.
	for i := 0; i < 5; i++ {
		startedAt := now.Add(-time.Duration(4-i) * time.Hour)
		s.Append(makeRun(jobID, startedAt))
	}

	// Warm cache via no-cutoff page. count is now keepCount = 5.
	if got := s.List(jobID, 5, time.Time{}); len(got) != 5 {
		t.Fatalf("warm List len=%d want 5", len(got))
	}

	// Now nuke the on-disk dir to prove the next call reads only from
	// cache. trim hasn't fired (count==keepCount, no over-cap), so
	// without the optimization the count==keepCount bail would force
	// a disk scan and observe an empty directory.
	dir := filepath.Join(s.root, jobID)
	if err := removeRunsDir(t, dir); err != nil {
		t.Fatalf("removeRunsDir: %v", err)
	}

	// cutoff = now-1h. Rows < cutoff in cache (newest first):
	// now-2h, now-3h, now-4h. Request limit=2 — page fills at index 2
	// (which is index < count=5), so the optimization can serve.
	cutoff := now.Add(-1 * time.Hour)
	got := s.List(jobID, 2, cutoff)
	if len(got) != 2 {
		t.Fatalf("List from cache at cap with filling page: len=%d want 2; got %+v", len(got), got)
	}
	for _, sm := range got {
		if !sm.StartedAt.Before(cutoff) {
			t.Fatalf("entry StartedAt %v >= cutoff %v", sm.StartedAt, cutoff)
		}
	}
}

// TestRunStore_ListBeforeCutoffFallsBackAtCapWhenPageDoesNotFill pins the
// safety side of R242-PERF-9 (#672): when the cache is at cap and the
// page does NOT fill (or fills only by walking to the oldest slot), the
// optimization falls back to disk so trim-evicted rows can be surfaced.
func TestRunStore_ListBeforeCutoffFallsBackAtCapWhenPageDoesNotFill(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 3, 30*24*time.Hour)
	s.enableTrimGC = false
	jobID := mustGenerateID()

	now := time.Now()
	// 5 inserts with keepCount=3 → cache mirrors the 3 newest, disk
	// keeps all 5 (trim disabled).
	for i := 0; i < 5; i++ {
		startedAt := now.Add(-time.Duration(4-i) * time.Hour)
		s.Append(makeRun(jobID, startedAt))
	}

	// Warm cache. cache count == 3, holds {now, now-1h, now-2h}.
	_ = s.List(jobID, 3, time.Time{})

	// cutoff = now-2h: cache has zero matches (all >= cutoff). Disk
	// holds two rows < cutoff (now-3h, now-4h) that the disk fallback
	// must surface.
	cutoff := now.Add(-2 * time.Hour)
	got := s.List(jobID, 10, cutoff)
	if len(got) != 2 {
		t.Fatalf("expected disk fallback to return 2 rows, got %d: %+v", len(got), got)
	}
}
