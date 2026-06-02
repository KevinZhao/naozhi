package cron

import (
	"testing"
	"time"
)

// TestCacheTrimAfterDisk_WrapClear pins R20260602190132-PERF-10: the
// two-segment clear() optimisation must zero exactly the evicted logical
// slots even when the evicted physical region wraps around the ring
// boundary.
//
// Strategy: seed a ring with cap=8 and head positioned so that after
// trim the surviving [0,survive) physical region ends before cap but
// the evicted [survive,count) region straddles the cap boundary.
//
// Ring cap = 8, count = 8 (full), head = 6.
//
// Physical layout (H = head = 6):
//
//	index:   0  1  2  3  4  5  6  7
//	logical: 2  3  4  5  6  7  0  1
//
// Newest-first logical order: 0,1,2,...,7 → physical 6,7,0,1,2,3,4,5.
// Cutoff drops logical slots 3..7 (survive=3, evicted=5).
// Evicted physical start = (6+3)%8 = 1; span = 5 → physical 1,2,3,4,5.
// No wrap needed for this evicted region (1+5=6 ≤ 8).
//
// To force a WRAP in the evicted region we set head=5, count=8, survive=2.
// Evicted logical [2,8) → physical start=(5+2)%8=7, span=6 → 7,0,1,2,3,4.
// 7+6=13 > 8 → two segments: ring[7:8] and ring[0:5].  ✓
func TestCacheTrimAfterDisk_WrapClear(t *testing.T) {
	t.Parallel()

	const ringCap = 8
	now := time.Now()

	// Build rows newest-first: row 0 is newest (now), row 7 is oldest (now-7m).
	rows := make([]CronRunSummary, ringCap)
	for i := 0; i < ringCap; i++ {
		rows[i] = CronRunSummary{
			RunID:     mustGenerateRunID(),
			JobID:     "aabbccddaabbccdd",
			State:     RunStateSucceeded,
			StartedAt: now.Add(-time.Duration(i) * time.Minute),
			EndedAt:   now.Add(-time.Duration(i) * time.Minute),
		}
	}

	// ringSeed places head=0 for a full ring. We then manually rotate head
	// to 5 so the evicted region [2,8) wraps around the physical boundary.
	entry := &recentCacheEntry{}
	entry.ringSeed(rows, ringCap)
	entry.warm = true
	entry.count = ringCap

	// Rotate: shift head from 0 to 5 by performing 5 ringPushHead no-ops
	// (we overwrite the ring slots to simulate a head shift, preserving
	// logical order). The simplest approach: rebuild the ring manually with
	// head=5.
	//
	// With head=5, logical slot i lives at ring[(5+i)%8]:
	//   logical 0 → physical 5 (newest, now)
	//   logical 1 → physical 6
	//   logical 2 → physical 7
	//   logical 3 → physical 0
	//   logical 4 → physical 1
	//   logical 5 → physical 2
	//   logical 6 → physical 3
	//   logical 7 → physical 4 (oldest, now-7m)
	ring := make([]CronRunSummary, ringCap)
	for logIdx, row := range rows {
		physIdx := (5 + logIdx) % ringCap
		ring[physIdx] = row
	}
	entry.ring = ring
	entry.head = 5

	// Verify setup: logical read must return rows in newest-first order.
	for i := 0; i < ringCap; i++ {
		got := entry.ringRead(i)
		if got.RunID != rows[i].RunID {
			t.Fatalf("setup: logical slot %d: got RunID %s, want %s", i, got.RunID, rows[i].RunID)
		}
	}

	s := newTestStore(t, ringCap, 30*24*time.Hour)
	jobID := "aabbccddaabbccdd"
	s.recentCache.Store(jobID, entry)

	// survive=2: keep logical slots 0,1; evict logical slots 2..7.
	// Cutoff = now-2m+1ns so rows 2..7 (ts=now-2m..now-7m) satisfy
	// ts.Before(cutoff) → evicted.
	cutoff := now.Add(-2*time.Minute + 1*time.Nanosecond)
	s.cacheTrimAfterDisk(jobID, cutoff)

	// 1. Surviving count must be 2.
	if entry.count != 2 {
		t.Fatalf("count = %d, want 2", entry.count)
	}

	// 2. Surviving logical slots 0 and 1 must be unchanged.
	for i := 0; i < 2; i++ {
		got := entry.ringRead(i)
		if got.RunID != rows[i].RunID {
			t.Fatalf("surviving slot %d: RunID = %s, want %s", i, got.RunID, rows[i].RunID)
		}
	}

	// 3. The evicted physical slots must be zero-valued (all fields zero).
	// Evicted logical [2,8) → physical (5+2)%8=7, span 6 → wraps: 7, 0,1,2,3,4.
	evictedPhysical := []int{7, 0, 1, 2, 3, 4}
	zero := CronRunSummary{}
	for _, phys := range evictedPhysical {
		if entry.ring[phys] != zero {
			t.Fatalf("evicted physical slot %d not zeroed: %+v", phys, entry.ring[phys])
		}
	}

	// 4. The surviving physical slots must still hold their data.
	// Surviving logical [0,2) → physical 5, 6.
	survivingPhysical := []struct {
		phys   int
		logIdx int
	}{
		{5, 0},
		{6, 1},
	}
	for _, sp := range survivingPhysical {
		if entry.ring[sp.phys].RunID != rows[sp.logIdx].RunID {
			t.Fatalf("surviving physical slot %d corrupted: RunID=%s want %s",
				sp.phys, entry.ring[sp.phys].RunID, rows[sp.logIdx].RunID)
		}
	}
}

// TestCacheTrimAfterDisk_WrapClear_NoEviction checks the degenerate case
// where survive==count (nothing to evict) — the clear path must be skipped
// entirely and the ring left untouched.
func TestCacheTrimAfterDisk_WrapClear_NoEviction(t *testing.T) {
	t.Parallel()

	const ringCap = 4
	now := time.Now()

	rows := make([]CronRunSummary, ringCap)
	for i := 0; i < ringCap; i++ {
		rows[i] = CronRunSummary{
			RunID:     mustGenerateRunID(),
			JobID:     "11223344aabbccdd",
			State:     RunStateSucceeded,
			StartedAt: now.Add(-time.Duration(i) * time.Minute),
			EndedAt:   now.Add(-time.Duration(i) * time.Minute),
		}
	}

	entry := &recentCacheEntry{}
	entry.ringSeed(rows, ringCap)
	entry.warm = true
	entry.count = ringCap

	s := newTestStore(t, ringCap, 30*24*time.Hour)
	jobID := "11223344aabbccdd"
	s.recentCache.Store(jobID, entry)

	// Cutoff far in the past → no row is before cutoff → survive=count=4.
	cutoff := now.Add(-100 * time.Hour)
	s.cacheTrimAfterDisk(jobID, cutoff)

	if entry.count != ringCap {
		t.Fatalf("count = %d, want %d (no eviction expected)", entry.count, ringCap)
	}
	for i := 0; i < ringCap; i++ {
		got := entry.ringRead(i)
		if got.RunID != rows[i].RunID {
			t.Fatalf("slot %d mutated unexpectedly: RunID=%s want %s", i, got.RunID, rows[i].RunID)
		}
	}
}

// TestCacheTrimAfterDisk_WrapClear_AllEvicted checks that evicting every
// slot (survive=0) zeroes the entire ring regardless of head position.
func TestCacheTrimAfterDisk_WrapClear_AllEvicted(t *testing.T) {
	t.Parallel()

	const ringCap = 6
	now := time.Now()

	rows := make([]CronRunSummary, ringCap)
	for i := 0; i < ringCap; i++ {
		rows[i] = CronRunSummary{
			RunID:     mustGenerateRunID(),
			JobID:     "ffeeddccbbaa9988",
			State:     RunStateSucceeded,
			StartedAt: now.Add(-time.Duration(i) * time.Minute),
			EndedAt:   now.Add(-time.Duration(i) * time.Minute),
		}
	}

	entry := &recentCacheEntry{}
	entry.ringSeed(rows, ringCap)
	entry.warm = true
	entry.count = ringCap
	// Shift head to 4 to force wrap when all slots are evicted.
	ring := make([]CronRunSummary, ringCap)
	for logIdx, row := range rows {
		ring[(4+logIdx)%ringCap] = row
	}
	entry.ring = ring
	entry.head = 4

	s := newTestStore(t, ringCap, 30*24*time.Hour)
	jobID := "ffeeddccbbaa9988"
	s.recentCache.Store(jobID, entry)

	// Cutoff in future → all rows evicted.
	cutoff := now.Add(1 * time.Minute)
	s.cacheTrimAfterDisk(jobID, cutoff)

	if entry.count != 0 {
		t.Fatalf("count = %d, want 0", entry.count)
	}
	zero := CronRunSummary{}
	for i := 0; i < ringCap; i++ {
		if entry.ring[i] != zero {
			t.Fatalf("physical slot %d not zeroed after full eviction: %+v", i, entry.ring[i])
		}
	}
}
