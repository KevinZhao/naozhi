package cron

import (
	"testing"
	"time"
)

// TestCacheTrimAfterDisk_BinarySearchEquivalence pins the R249-PERF-9
// (#930) refactor: cacheTrimAfterDisk's survive-boundary computation
// switched from a linear `for i := 0; i < limit; ...` loop to
// sort.Search. The cutover only changes algorithmic complexity (O(N) →
// O(log N)) while the predicate (`ts.Before(cutoff)` on a newest-first
// ring) is unchanged — but a regression in the predicate translation
// would silently mis-trim cache rows.
//
// Build a recentCacheEntry seeded with a known sequence of 10
// EndedAt-stamped CronRunSummary rows in newest-first order; choose a
// cutoff that splits the sequence in the middle. The expected survive
// count is the number of rows whose ts >= cutoff (i.e. NOT
// ts.Before(cutoff)). Pre-fix and post-fix must agree on this number,
// so the test is the regression guard for "future tweak to the
// predicate broke the boundary".
func TestCacheTrimAfterDisk_BinarySearchEquivalence(t *testing.T) {
	t.Parallel()

	const total = 10
	now := time.Now()
	// Rows 0..9 are newest..oldest, with ts = now, now-1m, ..., now-9m.
	// trimAfterDisk drops rows where ts.Before(cutoff) — i.e. survive
	// holds the prefix where ts >= cutoff.
	rows := make([]CronRunSummary, total)
	for i := 0; i < total; i++ {
		rows[i] = CronRunSummary{
			RunID:     mustGenerateRunID(),
			JobID:     "deadbeefdeadbeef",
			State:     RunStateSucceeded,
			StartedAt: now.Add(-time.Duration(i) * time.Minute),
			EndedAt:   now.Add(-time.Duration(i) * time.Minute),
		}
	}

	cases := []struct {
		name         string
		cutoffOffset time.Duration // cutoff = now + cutoffOffset
		wantSurvive  int
	}{
		{
			// Cutoff in the FAR PAST (-100h): every row has ts >= cutoff
			// → all 10 survive (none ts.Before(cutoff)).
			name:         "far-past-cutoff-keeps-all",
			cutoffOffset: -100 * time.Hour,
			wantSurvive:  total,
		},
		{
			// Cutoff at now-9m+1ns: row index 9 (oldest, ts=now-9m) has
			// ts.Before(cutoff) → dropped; rows 0..8 survive.
			name:         "drop-oldest",
			cutoffOffset: -9*time.Minute + 1*time.Nanosecond,
			wantSurvive:  total - 1,
		},
		{
			// Cutoff at now-5m+1ns: rows 5..9 have ts < cutoff → dropped;
			// rows 0..4 survive (5 total).
			name:         "mid-cutoff-keeps-newest-5",
			cutoffOffset: -5*time.Minute + 1*time.Nanosecond,
			wantSurvive:  5,
		},
		{
			// Cutoff at now+1m (future): every row has ts < cutoff →
			// dropped, survive=0.
			name:         "future-cutoff-drops-all",
			cutoffOffset: 1 * time.Minute,
			wantSurvive:  0,
		},
		{
			// Cutoff exactly equal to row 5's ts: ts.Before(cutoff) is
			// strict, so row 5 (ts == cutoff) is NOT before → survives.
			// Rows 6..9 (older) are before → dropped. 6 survive.
			name:         "exact-equal-survives",
			cutoffOffset: -5 * time.Minute,
			wantSurvive:  6,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t, total, 30*24*time.Hour)
			jobID := "deadbeefdeadbeef"

			// Manually seed the cache without going through Append so
			// the test pins the trim-side algorithm in isolation.
			entry := &recentCacheEntry{}
			entry.ringSeed(rows, total)
			entry.warm = true
			entry.count = total
			s.recentCache.Store(jobID, entry)

			cutoff := now.Add(tc.cutoffOffset)
			s.cacheTrimAfterDisk(jobID, cutoff)

			gotEntry, ok := s.recentCache.Load(jobID)
			if !ok {
				t.Fatalf("cache entry vanished")
			}
			gotCount := gotEntry.(*recentCacheEntry).count
			if gotCount != tc.wantSurvive {
				t.Fatalf("%s: survive count = %d, want %d (cutoff offset=%v)",
					tc.name, gotCount, tc.wantSurvive, tc.cutoffOffset)
			}
		})
	}
}

// TestCacheTrimAfterDisk_PreservesOrdering pins that the binary-search
// rewrite still leaves the surviving prefix in newest-first order — the
// invariant downstream callers (List / Recent) depend on. Trim with a
// cutoff that drops the oldest 3 rows; assert the surviving 7 are still
// monotonically non-increasing in EndedAt.
func TestCacheTrimAfterDisk_PreservesOrdering(t *testing.T) {
	t.Parallel()
	const total = 10
	now := time.Now()
	rows := make([]CronRunSummary, total)
	for i := 0; i < total; i++ {
		rows[i] = CronRunSummary{
			RunID:     mustGenerateRunID(),
			JobID:     "cafebabecafebabe",
			State:     RunStateSucceeded,
			StartedAt: now.Add(-time.Duration(i) * time.Minute),
			EndedAt:   now.Add(-time.Duration(i) * time.Minute),
		}
	}
	s := newTestStore(t, total, 30*24*time.Hour)
	jobID := "cafebabecafebabe"
	entry := &recentCacheEntry{}
	entry.ringSeed(rows, total)
	entry.warm = true
	entry.count = total
	s.recentCache.Store(jobID, entry)

	// Drop oldest 3 rows (rows 7, 8, 9): cutoff = now-7m + 1ns means
	// rows 7, 8, 9 (ts = now-7m, now-8m, now-9m) all satisfy
	// ts.Before(cutoff) → dropped. Rows 0..6 survive (7 total).
	cutoff := now.Add(-7*time.Minute + 1*time.Nanosecond)
	s.cacheTrimAfterDisk(jobID, cutoff)

	got := entry.ringSnapshot(0)
	if len(got) != 7 {
		t.Fatalf("snapshot len = %d, want 7", len(got))
	}
	for i := 0; i < len(got)-1; i++ {
		if got[i].EndedAt.Before(got[i+1].EndedAt) {
			t.Fatalf("ordering violated at %d: %v < %v", i, got[i].EndedAt, got[i+1].EndedAt)
		}
	}
}
