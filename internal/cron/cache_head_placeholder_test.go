package cron

import (
	"testing"
	"time"
)

// TestCacheHeadPush_PlaceholderOnAppend pins R246-GO-9 (#702): the prior
// cacheHeadPush implementation returned silently when the recentCache had
// no entry for jobID. The next cacheGet then ran the LoadOrStore +
// recentCacheEntry alloc itself, paying the placeholder cost out of band.
//
// The fix lifts the LoadOrStore into Append's hot path: by the time Append
// returns, the cache map MUST have an entry for jobID, even though the
// entry stays warm=false until the first cacheGet drives warmCache. The
// observable behaviour change is purely a placeholder lifecycle move; the
// snapshot returned to callers is unchanged (still goes through warmCache
// reading every run.json from disk on first miss).
//
// Test plan:
//  1. After Append on a virgin runStore (no prior cache load), recentCache
//     MUST contain the placeholder entry.
//  2. Placeholder MUST stay warm=false (so cacheGet still does the disk
//     warm pass — we have NOT seeded a partial cache that would miss
//     pre-restart history rows).
//  3. Subsequent cacheGet on the same jobID MUST observe the same entry
//     (no double-allocation), then transition warm=true after warmCache.
func TestCacheHeadPush_PlaceholderOnAppend(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 0, 0)
	jobID := mustGenerateID()

	run := makeRun(jobID, time.Now())
	s.Append(run)

	v, ok := s.recentCache.Load(jobID)
	if !ok {
		t.Fatalf("Append did not LoadOrStore a placeholder for jobID %q", jobID)
	}
	entry := v.(*recentCacheEntry)
	entry.mu.Lock()
	if entry.warm {
		t.Errorf("placeholder must stay warm=false until cacheGet drives warmCache")
	}
	entry.mu.Unlock()

	// Subsequent cacheGet must reuse the same placeholder pointer. Use the
	// internal map identity to assert no second alloc happened — this is
	// the primary win of the fix: cacheGet's hit path skips its own
	// LoadOrStore.
	rows, hit := s.cacheGet(jobID, 50)
	if !hit {
		t.Fatalf("cacheGet should hit after warm pass")
	}
	if len(rows) != 1 || rows[0].RunID != run.RunID {
		t.Fatalf("expected the just-Appended row, got %+v", rows)
	}
	v2, _ := s.recentCache.Load(jobID)
	if v2.(*recentCacheEntry) != entry {
		t.Errorf("cacheGet allocated a NEW entry for jobID %q — placeholder reuse contract broken", jobID)
	}
}

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
