package cron

import (
	"testing"
	"time"
)

// TestCacheGet_MissesWhenInvalidateRacesPostWarm pins R20260610-GO-007
// (#2000): when cacheInvalidate (DeleteJob path) deletes the recentCache
// key AFTER warmCache has populated it but BEFORE cacheGet's post-warm
// re-Load, the re-Load returns ok=false. Pre-fix, cacheGet silently fell
// through and kept the pre-warm `entry` pointer — and because warmCache's
// LoadOrStore can reuse that exact pointer, the freshly-warmed rows of the
// job being deleted were served as a valid result. Post-fix, cacheGet
// treats the vanished key as a miss and returns (nil, false): the job is
// being deleted, so a cache miss is the correct semantic.
//
// The race window is deterministic here via the cacheGetPostWarmHook test
// seam, which fires between warmCache and the re-Load. This is the sibling
// of TestCacheGet_ReLoadsAfterWarmCacheInvalidateRace (R247-GO-6 / #483),
// which covers the invalidate landing BEFORE warmCache instead.
func TestCacheGet_MissesWhenInvalidateRacesPostWarm(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 10, time.Hour)
	jobID := mustGenerateID()

	// Seed a run on disk so warmCache populates the ring with real rows —
	// the exact data the pre-fix code would have leaked past the delete.
	run := makeRun(jobID, time.Now())
	s.Append(run)

	// Drop the cache entry so cacheGet takes the cold path through
	// warmCache, then inject the DeleteJob-style invalidate into the
	// warmCache→re-Load window.
	s.recentCache.Delete(jobID)
	s.cacheGetPostWarmHook = func(id string) {
		if id == jobID {
			s.cacheInvalidate(jobID)
		}
	}
	defer func() { s.cacheGetPostWarmHook = nil }()

	got, ok := s.cacheGet(jobID, 10)
	if ok {
		t.Fatalf("cacheGet returned ok=true with %d rows after cacheInvalidate raced the post-warm re-Load; want (nil, false) miss for a job being deleted (#2000)", len(got))
	}
	if got != nil {
		t.Fatalf("cacheGet returned non-nil slice (%d rows) on miss; want nil (#2000)", len(got))
	}
}

// TestCacheGet_PostWarmHookNilByDefault pins that the hook seam stays a
// pure no-op in the normal cold path: with the hook unset, cacheGet warms
// and returns the disk row exactly as before the #2000 fix.
func TestCacheGet_PostWarmHookNilByDefault(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 10, time.Hour)
	jobID := mustGenerateID()
	run := makeRun(jobID, time.Now())
	s.Append(run)
	s.recentCache.Delete(jobID)

	got, ok := s.cacheGet(jobID, 10)
	if !ok {
		t.Fatalf("cold-path cacheGet returned ok=false with hook unset; warm pass regression")
	}
	if len(got) != 1 || got[0].RunID != run.RunID {
		t.Fatalf("cold-path cacheGet returned %d rows, want 1 row with RunID=%q", len(got), run.RunID)
	}
}
