// append_trim_cache_gate_test.go pins R20260527-PERF-24 (#1295):
// runStore.Append's force-trim every appendTrimBatch calls must
// consult the cache predicate (count + window) before descending into
// the ReadDir-walking trimJobLocked. Otherwise every 10th Append on a
// healthy job triggers a wasted ReadDir + Stat-per-entry walk over the
// whole runs/<jobID>/ tree (~14400 wasted disk walks per day at 50
// jobs × 1 Hz).
package cron

import (
	"testing"
	"time"
)

// TestSkipAppendTrim_ForceTrimRespectsCachePredicate drives the
// appendTrimBatch boundary on a job whose cache shows count = 1 (far
// below keepCount) and oldest mtime well within keepWindow. Pre-fix,
// every 10th call returned false (do trim) regardless. Post-fix, the
// cache predicate gates the descent: the call returns true (skip) so
// no ReadDir runs.
func TestSkipAppendTrim_ForceTrimRespectsCachePredicate(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 30*24*time.Hour)
	jobID := mustGenerateID()

	// Seed a single Append, then call Recent so warmCache fires and
	// the cache is lazy-warmed from disk. Append on a cold cache is a
	// no-op via cacheHeadPush — the warm happens on first Recent.
	s.Append(makeRun(jobID, time.Now()))
	_ = s.Recent(jobID, 10)

	v, ok := s.recentCache.Load(jobID)
	if !ok {
		t.Fatalf("precondition: cache should be warm after first Append+Recent")
	}
	entry := v.(*recentCacheEntry)
	entry.mu.Lock()
	if !entry.warm {
		entry.mu.Unlock()
		t.Fatalf("precondition: cache should be warm")
	}
	if entry.count != 1 {
		entry.mu.Unlock()
		t.Fatalf("precondition: cache count should be 1, got %d", entry.count)
	}
	entry.mu.Unlock()

	// Drive appendTrimBatch-1 calls under jobLock — none should force
	// the disk walk because count is far below keepCount and oldest
	// is well inside keepWindow.
	lock := s.jobLock(jobID)
	for i := 0; i < appendTrimBatch-1; i++ {
		lock.Lock()
		if !s.skipAppendTrim(jobID) {
			lock.Unlock()
			t.Fatalf("call %d: skipAppendTrim returned false (do trim) — should skip when cache says nothing to evict", i)
		}
		lock.Unlock()
	}

	// The appendTrimBatch boundary call: pre-fix returned false; post-fix
	// returns true because the cache predicate (count + window) says no.
	lock.Lock()
	skipAtBoundary := s.skipAppendTrim(jobID)
	lock.Unlock()
	if !skipAtBoundary {
		t.Errorf("appendTrimBatch boundary: skipAppendTrim returned false — should skip when cache says nothing to evict (R20260527-PERF-24 / #1295)")
	}
}

// TestSkipAppendTrim_ForceTrimAtCountCap drives a job whose cache
// reflects count near keepCount; the force-trim boundary MUST descend
// to trimJobLocked because the cap-violation predicate is true.
func TestSkipAppendTrim_ForceTrimAtCountCap(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 5, 30*24*time.Hour) // tiny cap
	s.enableTrimGC = false                   // suppress real trim
	jobID := mustGenerateID()
	now := time.Now()
	for i := 0; i < 5; i++ {
		s.Append(makeRun(jobID, now.Add(time.Duration(i)*time.Second)))
	}
	_ = s.Recent(jobID, 10) // warm cache

	v, ok := s.recentCache.Load(jobID)
	if !ok {
		t.Fatalf("precondition: cache should exist")
	}
	entry := v.(*recentCacheEntry)
	entry.mu.Lock()
	if entry.count != 5 {
		entry.mu.Unlock()
		t.Fatalf("precondition: cache count should be 5 (== keepCount), got %d", entry.count)
	}
	entry.mu.Unlock()

	// At count == keepCount, count+appendTrimBatch >= keepCount is
	// trivially true: every call should descend to trim.
	lock := s.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()
	if s.skipAppendTrim(jobID) {
		t.Errorf("skipAppendTrim returned true at cap — must descend to trim when cache predicate fires")
	}
}

// TestSkipAppendTrim_ForceTrimAtWindowExpiry drives a job whose
// oldest cached row is older than keepWindow. The force-trim path
// must descend even though count is well below keepCount.
func TestSkipAppendTrim_ForceTrimAtWindowExpiry(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 1*time.Second)
	s.enableTrimGC = false
	jobID := mustGenerateID()

	// Seed a single old row (StartedAt 1h ago) so the cache predicate
	// fires on the window arm.
	s.Append(makeRun(jobID, time.Now().Add(-1*time.Hour)))
	_ = s.Recent(jobID, 10) // warm cache

	lock := s.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()
	if s.skipAppendTrim(jobID) {
		t.Errorf("skipAppendTrim returned true with oldest row past keepWindow — must descend to trim")
	}
}
