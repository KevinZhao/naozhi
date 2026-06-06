package cron

import (
	"testing"
	"time"
)

// TestRingSeed_ClearTail pins R20260606-PERF-6: when ringSeed reuses an
// existing ring (cap == keepCount) with fewer rows than keepCount, the
// trailing slots beyond len(rows) must be zeroed so old string pointers
// are not retained.  The fix replaced a per-element zero loop with
// clear(e.ring[n:]).
func TestRingSeed_ClearTail(t *testing.T) {
	t.Parallel()

	const keepCount = 5
	now := time.Now()

	// Build 5 rows with distinct RunIDs.
	full := make([]CronRunSummary, keepCount)
	for i := range full {
		full[i] = CronRunSummary{
			RunID:     mustGenerateRunID(),
			JobID:     "aabbccdd11223344",
			StartedAt: now.Add(-time.Duration(i) * time.Minute),
		}
	}

	e := &recentCacheEntry{}

	// First seed: fill ring to capacity so all slots hold data.
	e.ringSeed(full, keepCount)
	if e.count != keepCount {
		t.Fatalf("first seed: count=%d want %d", e.count, keepCount)
	}

	// Second seed: only 2 rows — the remaining 3 physical slots must be zeroed.
	partial := full[:2]
	e.ringSeed(partial, keepCount)

	if e.count != 2 {
		t.Fatalf("partial seed: count=%d want 2", e.count)
	}
	// head must be reset to 0 by ringSeed.
	if e.head != 0 {
		t.Fatalf("partial seed: head=%d want 0", e.head)
	}

	// Slots [0,2) must hold the seeded rows.
	for i := 0; i < 2; i++ {
		got := e.ringRead(i)
		if got.RunID != partial[i].RunID {
			t.Fatalf("slot %d: RunID=%s want %s", i, got.RunID, partial[i].RunID)
		}
	}

	// Slots [2,keepCount) must be zero-valued (no lingering string pointers).
	zero := CronRunSummary{}
	for i := 2; i < keepCount; i++ {
		if e.ring[i] != zero {
			t.Fatalf("trailing slot %d not zeroed after partial reseed: %+v", i, e.ring[i])
		}
	}
}

// TestRingSeed_ClearTail_FullReseed verifies that when len(rows)==keepCount
// the clear path is skipped (no panic, no extra work) and all slots are
// populated correctly.
func TestRingSeed_ClearTail_FullReseed(t *testing.T) {
	t.Parallel()

	const keepCount = 4
	now := time.Now()

	rows := make([]CronRunSummary, keepCount)
	for i := range rows {
		rows[i] = CronRunSummary{
			RunID:     mustGenerateRunID(),
			StartedAt: now.Add(-time.Duration(i) * time.Minute),
		}
	}

	e := &recentCacheEntry{}
	e.ringSeed(rows, keepCount)
	// Re-seed with the same capacity — no trailing slots to clear.
	e.ringSeed(rows, keepCount)

	if e.count != keepCount {
		t.Fatalf("full reseed: count=%d want %d", e.count, keepCount)
	}
	for i := 0; i < keepCount; i++ {
		got := e.ringRead(i)
		if got.RunID != rows[i].RunID {
			t.Fatalf("slot %d: RunID=%s want %s", i, got.RunID, rows[i].RunID)
		}
	}
}

// TestRingSeed_ClearTail_EmptyReseed verifies that seeding with an empty
// slice zeroes all keepCount slots (edge case: len(rows)==0).
func TestRingSeed_ClearTail_EmptyReseed(t *testing.T) {
	t.Parallel()

	const keepCount = 3
	now := time.Now()

	rows := make([]CronRunSummary, keepCount)
	for i := range rows {
		rows[i] = CronRunSummary{
			RunID:     mustGenerateRunID(),
			StartedAt: now.Add(-time.Duration(i) * time.Minute),
		}
	}

	e := &recentCacheEntry{}
	// Seed with data first so the ring backing array is allocated and dirty.
	e.ringSeed(rows, keepCount)

	// Re-seed with empty rows.
	e.ringSeed(nil, keepCount)

	if e.count != 0 {
		t.Fatalf("empty reseed: count=%d want 0", e.count)
	}
	zero := CronRunSummary{}
	for i := 0; i < keepCount; i++ {
		if e.ring[i] != zero {
			t.Fatalf("slot %d not zeroed after empty reseed: %+v", i, e.ring[i])
		}
	}
}
