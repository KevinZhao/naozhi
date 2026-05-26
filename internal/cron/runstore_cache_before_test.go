package cron

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRunStore_ListBeforeCutoffServedFromCache verifies the R243-PERF-5
// (#810) fast-path: when the cache is warm AND has not hit keepCount,
// a before-cutoff List call returns the filtered slice without going
// to disk. Asserts correctness (filter is applied, equality boundary
// matches diskListNewestFirst) plus the no-disk-IO contract by removing
// the on-disk runs/<jobID>/ directory between warm and the second
// before-cutoff call.
func TestRunStore_ListBeforeCutoffServedFromCache(t *testing.T) {
	t.Parallel()
	// keepCount is intentionally large (200) so the cache stays under cap
	// for our 5 inserts, hitting the cacheGetBefore exhaustive branch.
	s := newTestStore(t, 200, 30*24*time.Hour)
	jobID := mustGenerateID()

	now := time.Now()
	for i := 0; i < 5; i++ {
		startedAt := now.Add(-time.Duration(4-i) * time.Hour)
		s.Append(makeRun(jobID, startedAt))
	}

	// Warm the cache with a no-cutoff List.
	if got := s.List(jobID, 50, time.Time{}); len(got) != 5 {
		t.Fatalf("warm List len=%d want 5", len(got))
	}

	// Now nuke the on-disk runs/<jobID>/ directory. If the
	// before-cutoff path goes to disk we'll observe an empty result;
	// the cache fast-path must return the in-memory filter answer.
	dir := filepath.Join(s.root, jobID)
	if err := removeRunsDir(t, dir); err != nil {
		t.Fatalf("removeRunsDir: %v", err)
	}

	// runs are at now-4h, now-3h, now-2h, now-1h, now; cutoff = now-2h.
	// Before-cutoff means StartedAt strictly < cutoff: 2 entries (now-4h, now-3h).
	cutoff := now.Add(-2 * time.Hour)
	got := s.List(jobID, 10, cutoff)
	if len(got) != 2 {
		t.Fatalf("List(before cutoff) from cache: len=%d want 2; got %+v", len(got), got)
	}
	for _, sm := range got {
		if !sm.StartedAt.Before(cutoff) {
			t.Fatalf("List returned entry with StartedAt %v not < cutoff %v", sm.StartedAt, cutoff)
		}
	}
}

// TestRunStore_ListBeforeCutoffFallsBackAtCap verifies that when the
// cache has hit keepCount (i.e. trimming may have evicted older rows),
// before-cutoff queries fall through to the disk scan. Without this
// guard a paginated query for entries older than the cache horizon
// would silently return a truncated set.
func TestRunStore_ListBeforeCutoffFallsBackAtCap(t *testing.T) {
	t.Parallel()
	// keepCount=3 small enough to overflow with our 5 inserts.
	s := newTestStore(t, 3, 30*24*time.Hour)
	// Disable the per-Append trim GC so all 5 files stay on disk —
	// we want to see disk vs. cache divergence.
	s.enableTrimGC = false
	jobID := mustGenerateID()

	now := time.Now()
	for i := 0; i < 5; i++ {
		startedAt := now.Add(-time.Duration(4-i) * time.Hour)
		run := makeRun(jobID, startedAt)
		s.Append(run)
	}

	// Warm with a no-cutoff page so cacheGetBefore sees warm=true,
	// count == keepCount=3, and bails out to disk.
	_ = s.List(jobID, 3, time.Time{})

	cutoff := now.Add(-2 * time.Hour)
	got := s.List(jobID, 10, cutoff)
	// runs at now-4h and now-3h are < cutoff; they live on disk but
	// not in the (now-saturated) cache. The disk fallback must
	// surface them. Without the cap guard the cache scan would miss
	// them entirely.
	if len(got) != 2 {
		t.Fatalf("List(before cutoff) at cache cap: len=%d want 2; got %+v", len(got), got)
	}
}

// removeRunsDir nukes runs/<jobID>/ recursively. Used by the cache-
// fast-path test to prove the answer came from cache without touching
// disk.
func removeRunsDir(t *testing.T, dir string) error {
	t.Helper()
	return os.RemoveAll(dir)
}
