package runstore

import (
	"testing"
	"time"
)

// TestCacheHeadPush_PlaceholderDoesNotPretendWarm guards a subtle
// regression risk: if a future fix ever decides to seed the placeholder
// with the just-Appended summary AND mark warm=true, the cache would
// return ONLY the new row on cacheGet — silently losing every disk-only
// row that predates process restart (warmCache would short-circuit on
// warm=true). Pin warm=false so any such "shortcut" trips this test.
func TestCacheHeadPush_PlaceholderDoesNotPretendWarm(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 0, 0)
	jobID := mustGenerateID()

	// Pre-seed a disk-only run BEFORE any cache touch — emulates the
	// "process restart with existing runs/<jobID>/ directory" scenario.
	older := makeRun(jobID, time.Now().Add(-time.Hour))
	s.Append(older)
	// Drop the cache entry to simulate post-restart cold state.
	s.recentCache.Delete(jobID)

	// Append a fresh run; placeholder should be installed but warm must
	// stay false so cacheGet re-reads disk and observes BOTH rows.
	newer := makeRun(jobID, time.Now())
	s.Append(newer)

	rows, ok := s.cacheGet(jobID, 50)
	if !ok {
		t.Fatalf("cacheGet should hit after warm pass")
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows (older + newer) after cacheGet warm pass, got %d", len(rows))
	}
}
