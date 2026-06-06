package cron

import (
	"testing"
	"time"
)

// TestCacheTrimAfterDisk_RunIDsShrink pins R20260604-CR-7: cacheTrimAfterDisk
// must delete evicted RunIDs from entry.runIDs so the dedup map stays in
// lockstep with the ring. Before the fix, trim only cleared ring slots but
// never touched runIDs, causing the map to grow monotonically.
//
// Strategy: seed a warm cache entry with N rows (all with populated runIDs),
// trim so only `survive` rows remain, then assert:
//  1. entry.count == survive
//  2. len(entry.runIDs) == survive  (no stale entries)
//  3. the surviving RunIDs are exactly those still in entry.runIDs
//  4. the evicted RunIDs are absent from entry.runIDs
func TestCacheTrimAfterDisk_RunIDsShrink(t *testing.T) {
	t.Parallel()

	const total = 8
	const survive = 3
	now := time.Now()

	rows := make([]CronRunSummary, total)
	for i := 0; i < total; i++ {
		rows[i] = CronRunSummary{
			RunID:     mustGenerateRunID(),
			JobID:     "aabbccddeeff0011",
			State:     RunStateSucceeded,
			StartedAt: now.Add(-time.Duration(i) * time.Minute),
			EndedAt:   now.Add(-time.Duration(i) * time.Minute),
		}
	}

	s := newTestStore(t, total, 30*24*time.Hour)
	jobID := "aabbccddeeff0011"
	entry := &recentCacheEntry{}
	entry.ringSeed(rows, total)
	entry.warm = true
	s.recentCache.Store(jobID, entry)

	// Sanity: ringSeed populates runIDs.
	if len(entry.runIDs) != total {
		t.Fatalf("pre-trim: len(runIDs) = %d, want %d", len(entry.runIDs), total)
	}

	// Cutoff that keeps the `survive` newest rows (logical indices 0..survive-1)
	// and evicts rows survive..total-1.
	// rows[survive].EndedAt = now - survive*minute; rows before it are newer.
	cutoff := now.Add(-time.Duration(survive)*time.Minute + 1*time.Nanosecond)
	s.cacheTrimAfterDisk(jobID, cutoff)

	// 1. entry.count matches expected survive count.
	if entry.count != survive {
		t.Fatalf("entry.count = %d, want %d", entry.count, survive)
	}

	// 2. runIDs map shrunk to match.
	if len(entry.runIDs) != survive {
		t.Fatalf("len(runIDs) = %d, want %d (stale evicted entries not removed)",
			len(entry.runIDs), survive)
	}

	// 3. Each surviving logical slot's RunID is present in runIDs.
	for i := 0; i < survive; i++ {
		rid := entry.ringRead(i).RunID
		if _, ok := entry.runIDs[rid]; !ok {
			t.Errorf("surviving RunID[%d] %s missing from runIDs", i, rid)
		}
	}

	// 4. Each evicted row's RunID is absent from runIDs.
	for i := survive; i < total; i++ {
		rid := rows[i].RunID
		if _, ok := entry.runIDs[rid]; ok {
			t.Errorf("evicted RunID[%d] %s still present in runIDs", i, rid)
		}
	}
}

// TestCacheTrimAfterDisk_RunIDsAllEvicted checks that trimming all rows
// leaves entry.runIDs empty (not just cleared of the right subset).
func TestCacheTrimAfterDisk_RunIDsAllEvicted(t *testing.T) {
	t.Parallel()

	const total = 5
	now := time.Now()

	rows := make([]CronRunSummary, total)
	for i := 0; i < total; i++ {
		rows[i] = CronRunSummary{
			RunID:     mustGenerateRunID(),
			JobID:     "112233445566aabb",
			State:     RunStateSucceeded,
			StartedAt: now.Add(-time.Duration(i) * time.Minute),
			EndedAt:   now.Add(-time.Duration(i) * time.Minute),
		}
	}

	s := newTestStore(t, total, 30*24*time.Hour)
	jobID := "112233445566aabb"
	entry := &recentCacheEntry{}
	entry.ringSeed(rows, total)
	entry.warm = true
	s.recentCache.Store(jobID, entry)

	// Future cutoff evicts everything.
	cutoff := now.Add(1 * time.Minute)
	s.cacheTrimAfterDisk(jobID, cutoff)

	if entry.count != 0 {
		t.Fatalf("count = %d, want 0", entry.count)
	}
	if len(entry.runIDs) != 0 {
		t.Fatalf("len(runIDs) = %d, want 0 after full eviction", len(entry.runIDs))
	}
}

// TestCacheTrimAfterDisk_RunIDsNoEviction checks that when survive==count
// (nothing evicted) runIDs is untouched.
func TestCacheTrimAfterDisk_RunIDsNoEviction(t *testing.T) {
	t.Parallel()

	const total = 4
	now := time.Now()

	rows := make([]CronRunSummary, total)
	for i := 0; i < total; i++ {
		rows[i] = CronRunSummary{
			RunID:     mustGenerateRunID(),
			JobID:     "ffeeddcc99887766",
			State:     RunStateSucceeded,
			StartedAt: now.Add(-time.Duration(i) * time.Minute),
			EndedAt:   now.Add(-time.Duration(i) * time.Minute),
		}
	}

	s := newTestStore(t, total, 30*24*time.Hour)
	jobID := "ffeeddcc99887766"
	entry := &recentCacheEntry{}
	entry.ringSeed(rows, total)
	entry.warm = true
	s.recentCache.Store(jobID, entry)

	// Far-past cutoff — nothing evicted.
	cutoff := now.Add(-100 * time.Hour)
	s.cacheTrimAfterDisk(jobID, cutoff)

	if entry.count != total {
		t.Fatalf("count = %d, want %d", entry.count, total)
	}
	if len(entry.runIDs) != total {
		t.Fatalf("len(runIDs) = %d, want %d (should be unchanged)", len(entry.runIDs), total)
	}
}

// TestCacheTrimAfterDisk_RunIDsWrapHead checks that the runIDs cleanup works
// correctly when the ring head is not at position 0 (wrap case), matching the
// wrap-clear test pattern in cachetrim_wrap_clear_test.go.
func TestCacheTrimAfterDisk_RunIDsWrapHead(t *testing.T) {
	t.Parallel()

	const ringCap = 8
	const survive = 2
	now := time.Now()

	rows := make([]CronRunSummary, ringCap)
	for i := 0; i < ringCap; i++ {
		rows[i] = CronRunSummary{
			RunID:     mustGenerateRunID(),
			JobID:     "deadbeefcafebabe",
			State:     RunStateSucceeded,
			StartedAt: now.Add(-time.Duration(i) * time.Minute),
			EndedAt:   now.Add(-time.Duration(i) * time.Minute),
		}
	}

	// Build entry with head=5 (forces physical wrap in evicted region).
	entry := &recentCacheEntry{}
	entry.ringSeed(rows, ringCap)
	ring := make([]CronRunSummary, ringCap)
	for logIdx, row := range rows {
		ring[(5+logIdx)%ringCap] = row
	}
	entry.ring = ring
	entry.head = 5
	entry.count = ringCap
	// Rebuild runIDs to match the rotated ring.
	entry.runIDs = make(map[string]struct{}, ringCap)
	for _, r := range rows {
		entry.runIDs[r.RunID] = struct{}{}
	}
	entry.warm = true

	s := newTestStore(t, ringCap, 30*24*time.Hour)
	jobID := "deadbeefcafebabe"
	s.recentCache.Store(jobID, entry)

	// Keep logical slots 0,1 (survive=2); evict logical slots 2..7.
	cutoff := now.Add(-time.Duration(survive)*time.Minute + 1*time.Nanosecond)
	s.cacheTrimAfterDisk(jobID, cutoff)

	if entry.count != survive {
		t.Fatalf("count = %d, want %d", entry.count, survive)
	}
	if len(entry.runIDs) != survive {
		t.Fatalf("len(runIDs) = %d, want %d", len(entry.runIDs), survive)
	}
	// Surviving RunIDs present.
	for i := 0; i < survive; i++ {
		rid := rows[i].RunID
		if _, ok := entry.runIDs[rid]; !ok {
			t.Errorf("surviving RunID[%d] %s missing from runIDs", i, rid)
		}
	}
	// Evicted RunIDs absent.
	for i := survive; i < ringCap; i++ {
		rid := rows[i].RunID
		if _, ok := entry.runIDs[rid]; ok {
			t.Errorf("evicted RunID[%d] %s still in runIDs", i, rid)
		}
	}
}
