package cron

import (
	"sync"
	"testing"
	"time"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// TestMissedScheduleVerdict_ConcurrentCacheHitNoRace pins R090135-PERF-001:
// missedCacheMu was a sync.Mutex, serialising all cache-hit reads even though
// reads are side-effect-free. After the change to sync.RWMutex, concurrent
// cache-hit readers must not race against each other.
//
// Run with -race to verify; the test fails at the data-race detector level
// before any assertion fires if the lock discipline is wrong.
func TestMissedScheduleVerdict_ConcurrentCacheHitNoRace(t *testing.T) {
	t.Parallel()

	h := &Handlers{}
	now := time.Now()
	startedAt := now.Add(-2 * time.Hour)
	j := &cronpkg.Job{
		ID:        "race-concurrent-read",
		Schedule:  "@every 1m",
		CreatedAt: now.Add(-1 * time.Hour),
	}

	// Prime the cache with one real computation so subsequent calls are
	// guaranteed cache hits within missedCacheTTL.
	missed0, prev0 := h.missedScheduleVerdict(j, now, startedAt)

	const goroutines = 50
	results := make([]struct {
		missed bool
		prev   time.Time
	}, goroutines)

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, p := h.missedScheduleVerdict(j, now, startedAt)
			results[i].missed = m
			results[i].prev = p
		}()
	}
	wg.Wait()

	for i, r := range results {
		if r.missed != missed0 {
			t.Errorf("goroutine %d: missed = %v, want %v", i, r.missed, missed0)
		}
		if !r.prev.Equal(prev0) {
			t.Errorf("goroutine %d: prevAt = %v, want %v", i, r.prev, prev0)
		}
	}
}
