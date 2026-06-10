package cron

import (
	"path/filepath"
	"testing"
)

// TestContainsSessionID_FastPathWarmsCache pins R20260609-COR-003 (#1978):
// a fast-path hit on Job.LastSessionID must still populate the TTL cache.
// Before the fix the fast path returned true without building/publishing, so
// a steady-state probe stream that always hit LastSessionID left the cache
// permanently cold — forcing the dashboard's 1Hz KnownSessionIDs() to pay the
// full O(jobs × recentCap) rebuild on every tick.
func TestContainsSessionID_FastPathWarmsCache(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath:      filepath.Join(dir, "cron.json"),
		MaxJobs:        5,
		AllowNilRouter: true,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	job := &Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "c", ChatType: "direct"}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	const lastSessionID = "fastpath-warm-aaaa-bbbb-cccc-000000000001"
	s.mu.Lock()
	s.jobs[job.ID].LastSessionID = lastSessionID
	s.mu.Unlock()

	// Cold the cache so the probe takes the fast path (not a warm lookupFresh).
	s.invalidateKnownSessionsCache()
	if _, ok := s.knownSessionsCache.lookupFresh(); ok {
		t.Fatal("precondition: cache must be cold before the fast-path probe")
	}

	// Fast-path hit on LastSessionID.
	if !s.containsSessionID(lastSessionID) {
		t.Fatalf("containsSessionID(%q) = false, want true (LastSessionID fast path)", lastSessionID)
	}

	// The fix: the fast-path hit must have warmed the TTL cache, so the next
	// lookupFresh serves from cache instead of cold-rebuilding.
	set, ok := s.knownSessionsCache.lookupFresh()
	if !ok {
		t.Fatal("cache still cold after fast-path hit — #1978 regression (fast path did not warm the TTL cache)")
	}
	if _, present := set[lastSessionID]; !present {
		t.Fatalf("warmed cache missing fast-path session id %q; set=%v", lastSessionID, set)
	}
}
