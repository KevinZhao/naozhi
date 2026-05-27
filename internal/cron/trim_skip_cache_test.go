// trim_skip_cache_test.go: regression coverage for R236-PERF-12 (#532).
//
// trimSkipFromCache short-circuits trimJobLocked's ReadDir + per-entry
// Stat when the recentCache state proves nothing on disk needs removal.
// The optimisation is purely additive: any path that would have hit
// scanSortedRunDir + cacheTrimAfterDisk before still does when the
// cache cannot vouch for disk state (cold cache, count >= keepCount,
// or oldest row at/below cutoff).

package cron

import (
	"testing"
	"time"
)

// seedWarmCache installs a warm recentCacheEntry for jobID with the
// supplied newest-first rows. Caller is responsible for any subsequent
// jobLock acquisition. Mirrors what warmCache(jobID) would have done
// after a real disk read.
func seedWarmCache(t *testing.T, s *runStore, jobID string, rows []CronRunSummary) {
	t.Helper()
	v, _ := s.recentCache.LoadOrStore(jobID, &recentCacheEntry{})
	entry := v.(*recentCacheEntry)
	entry.mu.Lock()
	entry.ringSeed(rows, s.keepCount)
	entry.warm = true
	entry.mu.Unlock()
}

// TestTrimSkipFromCache_HotPathSkipsScan asserts that with a warm cache,
// count well under keepCount, and oldest row newer than the keepWindow
// cutoff, trimSkipFromCache returns true so the caller can avoid the
// ReadDir.
func TestTrimSkipFromCache_HotPathSkipsScan(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 24*time.Hour)
	jobID := mustGenerateID()

	now := time.Now()
	seedWarmCache(t, s, jobID, []CronRunSummary{
		{
			RunID:     mustGenerateRunID(),
			StartedAt: now.Add(-time.Minute),
			EndedAt:   now.Add(-time.Minute + time.Second),
		},
	})

	// Caller of trimSkipFromCache must hold jobLock (asserted via
	// assertJobLockHeld inside trimJobLocked; trimSkipFromCache itself
	// only reads under entry.mu but the trim path is jobLock-serialised).
	lock := s.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()

	if !s.trimSkipFromCache(jobID, now) {
		t.Fatalf("expected trimSkipFromCache=true with warm cache + fresh oldest row")
	}
}

// TestTrimSkipFromCache_ColdCacheFallsThrough asserts the fast path is
// disabled until the cache has been explicitly warmed — a fresh entry
// (count=0, warm=false) must NOT be treated as "nothing to trim".
func TestTrimSkipFromCache_ColdCacheFallsThrough(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, 24*time.Hour)
	jobID := mustGenerateID()

	lock := s.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()

	if s.trimSkipFromCache(jobID, time.Now()) {
		t.Fatalf("expected trimSkipFromCache=false with no cache entry (cold)")
	}
}

// TestTrimSkipFromCache_OldestExpiredFallsThrough asserts the fast path
// declines to skip when the oldest cached row is at or older than the
// window cutoff — those entries need a real disk-side os.Remove pass.
func TestTrimSkipFromCache_OldestExpiredFallsThrough(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 200, time.Hour)
	jobID := mustGenerateID()

	now := time.Now()
	// Seed a row whose StartedAt is past the 1h cutoff.
	seedWarmCache(t, s, jobID, []CronRunSummary{
		{
			RunID:     mustGenerateRunID(),
			StartedAt: now.Add(-time.Hour - time.Second),
			EndedAt:   now.Add(-time.Hour - time.Second).Add(time.Second),
		},
	})

	lock := s.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()

	if s.trimSkipFromCache(jobID, now) {
		t.Fatalf("expected trimSkipFromCache=false with oldest row at/older than cutoff")
	}
}

// TestTrimSkipFromCache_CountAtKeepFallsThrough asserts the fast path
// stays off when the cache reaches keepCount: at that point older rows
// may have been rotated off the ring and the cache no longer enumerates
// every on-disk file — only a real ReadDir can prove the trim state.
func TestTrimSkipFromCache_CountAtKeepFallsThrough(t *testing.T) {
	t.Parallel()
	keepCount := 4
	s := newTestStore(t, keepCount, 24*time.Hour)
	jobID := mustGenerateID()

	now := time.Now()
	rows := make([]CronRunSummary, keepCount)
	for i := 0; i < keepCount; i++ {
		rows[i] = CronRunSummary{
			RunID:     mustGenerateRunID(),
			StartedAt: now.Add(-time.Duration(i+1) * time.Minute),
			EndedAt:   now.Add(-time.Duration(i+1)*time.Minute + time.Second),
		}
	}
	seedWarmCache(t, s, jobID, rows)

	lock := s.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()

	if s.trimSkipFromCache(jobID, now) {
		t.Fatalf("expected trimSkipFromCache=false with count == keepCount")
	}
}
