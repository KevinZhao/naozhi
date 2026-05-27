package cron

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRunStore_List_LongRunningJobInPaginatedPage is the public-API
// regression for the unsafe-as-proposed fast paths:
//
//   - R237-PERF-8 / #682 ("no mtime pre-filter — full JSON parse before
//     discard")
//   - R236-PERF-07 / #522 ("binary-search for `before` cutoff; ReadFile
//     only ones within the requested page")
//
// Both proposals would have skipped readRun for entries with mtime ≥
// before. The skip is unsafe: a long-running job whose StartedAt is
// before the cutoff but whose finishRun (or process-restart re-touch)
// landed AFTER the cutoff has mtime ≥ before yet must still appear in
// the page. The disk-level test
// (TestRunStore_DiskList_BeforeStartedAtMtimeDivergence) already pins
// diskListNewestFirst; this one pins the public List() entry-point so
// neither the cache fast path (cacheGetBefore) nor the disk fallback
// silently re-introduces an mtime gate.
func TestRunStore_List_LongRunningJobInPaginatedPage(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	s.enableTrimGC = false
	jobID := mustGenerateID()

	now := time.Now()
	// One short run that's clearly older than the cutoff.
	shortStart := now.Add(-3 * time.Hour)
	short := makeRun(jobID, shortStart)
	s.Append(short)

	// The long-running job: StartedAt 2h ago (before the cutoff at -1h)
	// but mtime forced to NOW so the entry's directory ordering by
	// mtime puts it at the head, masking it from a hypothetical
	// "skip mtime ≥ before" pre-filter.
	longStart := now.Add(-2 * time.Hour)
	long := makeRun(jobID, longStart)
	s.Append(long)
	longPath := filepath.Join(s.root, jobID, long.RunID+".json")
	if err := os.Chtimes(longPath, now, now); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Warm the cache with a no-cutoff page; since count (2) < keepCount
	// (200), cacheGetBefore is exhaustive and should serve the
	// before-cutoff query from cache without ever touching disk again.
	if got := s.List(jobID, 50, time.Time{}); len(got) != 2 {
		t.Fatalf("warm List len=%d want 2", len(got))
	}

	cutoff := now.Add(-1 * time.Hour)
	rows := s.List(jobID, 10, cutoff)
	// Both runs have StartedAt < cutoff so both belong in the page;
	// in particular the long-running one MUST NOT be skipped despite
	// its mtime ≥ cutoff.
	if len(rows) != 2 {
		t.Fatalf("List(before cutoff) len=%d want 2 — long-running run with mtime>=cutoff but StartedAt<cutoff was dropped", len(rows))
	}
	gotIDs := map[string]bool{rows[0].RunID: true, rows[1].RunID: true}
	if !gotIDs[short.RunID] {
		t.Fatalf("rows missing short run %s; got %+v", short.RunID, rows)
	}
	if !gotIDs[long.RunID] {
		t.Fatalf("rows missing long-running run %s; an mtime gate would have hidden it", long.RunID)
	}
}

// TestRunStore_List_LongRunningJobInPaginatedPage_DiskFallback covers
// the same #682/#522 regression on the disk-fallback path (cache cap
// reached). Without the strict StartedAt filter both proposals would
// also regress here.
func TestRunStore_List_LongRunningJobInPaginatedPage_DiskFallback(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 3, 30*24*time.Hour)
	s.enableTrimGC = false
	jobID := mustGenerateID()

	now := time.Now()
	// Three filler runs newer than the cutoff so the cache saturates
	// at keepCount=3 and List falls through to disk for the older
	// entries.
	for i := 0; i < 3; i++ {
		s.Append(makeRun(jobID, now.Add(-time.Duration(10*(i+1))*time.Minute)))
	}

	// The long-running job: StartedAt before the cutoff, mtime after.
	longStart := now.Add(-2 * time.Hour)
	long := makeRun(jobID, longStart)
	s.Append(long)
	longPath := filepath.Join(s.root, jobID, long.RunID+".json")
	if err := os.Chtimes(longPath, now, now); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Warm with a no-cutoff page so cacheGetBefore observes warm=true
	// + count==keepCount and falls through to disk.
	_ = s.List(jobID, 3, time.Time{})

	cutoff := now.Add(-1 * time.Hour)
	rows := s.List(jobID, 10, cutoff)
	found := false
	for _, sm := range rows {
		if sm.RunID == long.RunID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("disk-fallback List dropped long-running run %s — mtime gate regression", long.RunID)
	}
}
