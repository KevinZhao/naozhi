package cron

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCacheGetBefore_NoEarlyExitOnFirstMatch is a regression guard for #2323.
//
// #2323 proposed adding `if r.StartedAt.Before(before) { break }` after the
// append in cacheGetBefore, on the theory that "the ring is newest-first, so
// once a row satisfies StartedAt < before all later rows do too" and the scan
// can stop early. That reasoning is INVERTED: the before-cutoff filter KEEPS
// rows older than the cutoff (StartedAt < before) and SKIPS the newer ones via
// `continue`. In a newest-first ring the matching (older) rows are therefore a
// contiguous SUFFIX at the tail, not a prefix. Breaking on the first matching
// row would return only that single row and drop every older one behind it.
//
// This test reproduces the issue's own "busy job + before-cutoff skips the
// newest prefix" scenario: many recent rows that fail the cutoff (skipped at
// the head) followed by several older rows that pass it (the tail). The
// proposed break would return 1; the correct behaviour returns all matches up
// to limit. It also exercises a limit larger than the match count to prove the
// loop collects the entire qualifying suffix rather than stopping at the first
// match.
func TestCacheGetBefore_NoEarlyExitOnFirstMatch(t *testing.T) {
	t.Parallel()

	const keepCount = 500
	s := newTestStore(t, keepCount, 30*24*time.Hour)
	jobID := mustGenerateID()

	now := time.Now()
	// 100 recent rows (within the last ~100 seconds) — all FAIL the cutoff and
	// are skipped via `continue` at the head of the newest-first ring.
	for i := 0; i < 100; i++ {
		startedAt := now.Add(-time.Duration(i+1) * time.Second)
		s.Append(makeRun(jobID, startedAt))
	}
	// 30 older rows (hours ago) — all PASS the cutoff; they form the matching
	// suffix at the tail of the ring.
	for i := 0; i < 30; i++ {
		startedAt := now.Add(-time.Duration(i+3) * time.Hour)
		s.Append(makeRun(jobID, startedAt))
	}

	// Warm via a no-cutoff List so cacheGetBefore observes warm=true and the
	// cache stays under keepCount (exhaustive branch).
	if got := s.List(jobID, keepCount, time.Time{}); len(got) != 130 {
		t.Fatalf("warm List len=%d want 130", len(got))
	}

	// Nuke disk so any fallback would surface as an empty / short result and
	// we are strictly asserting the cache scan.
	dir := filepath.Join(s.root, jobID)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	cutoff := now.Add(-1 * time.Hour) // only the 30 older rows qualify.

	// limit larger than the match count: must return all 30 matches, NOT 1.
	// An inverted `break` on the first matching row would return exactly 1.
	got := s.List(jobID, 100, cutoff)
	if len(got) != 30 {
		t.Fatalf("List(limit=100, busy before-cutoff) len=%d want 30 (inverted early-exit regression?); got %d rows", len(got), len(got))
	}
	for _, sm := range got {
		if !sm.StartedAt.Before(cutoff) {
			t.Fatalf("returned row StartedAt %v not < cutoff %v", sm.StartedAt, cutoff)
		}
	}
}

// TestCacheGetBefore_OutOfOrderRingNoBreak guards the second, independent
// reason the #2323 break is unsound: the ring is ordered by Append/completion
// order (≈ EndedAt), NOT strictly by StartedAt. A long-running job can complete
// — and thus be pushed to the ring head — after a job that started later but
// finished sooner, so StartedAt is not guaranteed monotonic along the ring.
// diskListNewestFirst deliberately uses `continue` (not break) on the StartedAt
// filter for exactly this reason; cacheGetBefore must mirror it. Here a row
// that passes the cutoff sits BEFORE (newer in ring order) a row that fails it,
// so any StartedAt-based break would drop a qualifying row.
func TestCacheGetBefore_OutOfOrderRingNoBreak(t *testing.T) {
	t.Parallel()

	const keepCount = 50
	s := newTestStore(t, keepCount, 30*24*time.Hour)
	jobID := mustGenerateID()

	now := time.Now()
	cutoff := now.Add(-1 * time.Hour)

	// Append in completion order, which is the ring order (head = last pushed).
	// Mix qualifying (old, < cutoff) and non-qualifying (recent, >= cutoff)
	// rows so neither a prefix nor a suffix is purely one class:
	//   pushed #1 (tail): old      -> qualifies
	//   pushed #2:        recent   -> skipped
	//   pushed #3:        old      -> qualifies
	//   pushed #4 (head): recent   -> skipped
	s.Append(makeRun(jobID, now.Add(-3*time.Hour)))   // old
	s.Append(makeRun(jobID, now.Add(-5*time.Minute))) // recent
	s.Append(makeRun(jobID, now.Add(-2*time.Hour)))   // old
	s.Append(makeRun(jobID, now.Add(-1*time.Minute))) // recent

	if got := s.List(jobID, keepCount, time.Time{}); len(got) != 4 {
		t.Fatalf("warm List len=%d want 4", len(got))
	}

	dir := filepath.Join(s.root, jobID)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	// Both old rows qualify; the recent rows interleaved between them must be
	// skipped via `continue`, and the second old row (deeper in the ring) must
	// still be collected. A StartedAt break would stop at the first old row.
	got, ok := s.cacheGetBefore(jobID, 10, cutoff)
	if !ok {
		t.Fatalf("cacheGetBefore ok=false want true (warm, under cap)")
	}
	if len(got) != 2 {
		t.Fatalf("cacheGetBefore out-of-order ring len=%d want 2 (continue not break); got %+v", len(got), got)
	}
	for _, sm := range got {
		if !sm.StartedAt.Before(cutoff) {
			t.Fatalf("returned row StartedAt %v not < cutoff %v", sm.StartedAt, cutoff)
		}
	}
}
