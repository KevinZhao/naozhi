package cron

import (
	"testing"
	"time"
)

// TestCacheTrimAfterDisk_StaleEvictionTotal pins R249-CR-19 (#962):
// cacheTrimAfterDisk evicts cache rows using its EndedAt/StartedAt time-source
// approximation rather than the authoritative disk mtime. The eviction count
// must now feed cacheStaleEvictionTotal so the < ~1s pathological divergence
// the godoc documents becomes observable at runtime. Seeds a warm ring of 10
// rows, trims with a cutoff that drops the oldest 4, and asserts the counter
// advanced by exactly 4.
func TestCacheTrimAfterDisk_StaleEvictionTotal(t *testing.T) {
	t.Parallel()
	const total = 10
	now := time.Now()
	rows := make([]CronRunSummary, total)
	for i := 0; i < total; i++ {
		rows[i] = CronRunSummary{
			RunID:     mustGenerateRunID(),
			JobID:     "feedfacefeedface",
			State:     RunStateSucceeded,
			StartedAt: now.Add(-time.Duration(i) * time.Minute),
			EndedAt:   now.Add(-time.Duration(i) * time.Minute),
		}
	}
	s := newTestStore(t, total, 30*24*time.Hour)
	jobID := "feedfacefeedface"

	if got := s.CacheStaleEvictionTotal(); got != 0 {
		t.Fatalf("CacheStaleEvictionTotal before trim = %d want 0", got)
	}

	entry := &recentCacheEntry{}
	entry.ringSeed(rows, total)
	entry.warm = true
	entry.count = total
	s.recentCache.Store(jobID, entry)

	// Drop oldest 4 rows (indices 6,7,8,9): cutoff = now-6m + 1ns.
	cutoff := now.Add(-6*time.Minute + 1*time.Nanosecond)
	s.cacheTrimAfterDisk(jobID, cutoff)

	if got := entry.count; got != 6 {
		t.Fatalf("survive count = %d want 6", got)
	}
	if got := s.CacheStaleEvictionTotal(); got != 4 {
		t.Fatalf("CacheStaleEvictionTotal after trim = %d want 4", got)
	}

	// A no-op trim (cutoff far in the past keeps all) must NOT bump the
	// counter — the metric tracks actual evictions only.
	s.cacheTrimAfterDisk(jobID, now.Add(-100*time.Hour))
	if got := s.CacheStaleEvictionTotal(); got != 4 {
		t.Fatalf("CacheStaleEvictionTotal after no-op trim = %d want 4 (unchanged)", got)
	}
}
