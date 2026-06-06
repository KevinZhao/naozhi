package cli

import (
	"sync"
	"testing"
)

// TestEventLog_EntriesSince_ConcurrentAppend pins the R220-PERF-3 (#685)
// invariant: EntriesSince scans the ring under l.mu.RLock then releases
// the lock BEFORE the slices.Reverse step. Run with -race so the data
// races a buggy refactor would introduce (e.g. reversing on l.entries
// directly, or holding the lock across user-visible work) surface as
// race-detector hits.
//
// Pure correctness coverage — the perf gain itself is unobservable from
// userspace; this test exists so a future "simplify by re-deferring
// RUnlock" refactor cannot silently regress the lock-window discipline.
func TestEventLog_EntriesSince_ConcurrentAppend(t *testing.T) {
	t.Parallel()
	l := NewEventLog(500)
	// Seed enough entries that EntriesSince has something to reverse.
	for i := 0; i < 100; i++ {
		l.Append(EventEntry{Time: int64(i + 1), Type: "seed"})
	}

	var wg sync.WaitGroup
	const writers = 4
	const readers = 4
	const iters = 200

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				l.Append(EventEntry{Time: int64(base*iters + i + 1000), Type: "live"})
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				_ = l.EntriesSince(0)
			}
		}()
	}
	wg.Wait()
}

// TestEventLog_EntriesBefore_ConcurrentAppend pins the R053116-PERF-10
// invariant: EntriesBeforeAppend scans the ring under l.mu.RLock then
// releases the lock BEFORE the slices.Reverse step, matching the pattern
// established by EntriesSince (R220-PERF-3). Run with -race to surface
// data races that a "re-defer RUnlock" regression would introduce.
func TestEventLog_EntriesBefore_ConcurrentAppend(t *testing.T) {
	t.Parallel()
	l := NewEventLog(500)
	// Seed entries so EntriesBefore has something to scan and reverse.
	for i := 0; i < 100; i++ {
		l.Append(EventEntry{Time: int64(i + 1), Type: "seed"})
	}

	var wg sync.WaitGroup
	const writers = 4
	const readers = 4
	const iters = 200

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				l.Append(EventEntry{Time: int64(base*iters + i + 1000), Type: "live"})
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				// beforeMS=0 means "all entries" — exercises the full scan + Reverse.
				_ = l.EntriesBefore(0, 50)
			}
		}()
	}
	wg.Wait()
}
