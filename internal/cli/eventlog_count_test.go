package cli

// R202606-PERF-005: EventLog.Count() reads a lock-free atomic mirror of the
// `count` field. These tests pin the mirror's correctness (count == N below
// capacity, capped at maxSize after overwrite) and that concurrent Append +
// Count is race-free / panic-free under -race.

import (
	"sync"
	"testing"
)

func TestEventLog_Count_MirrorsAppends(t *testing.T) {
	t.Parallel()

	l := NewEventLog(8)
	if got := l.Count(); got != 0 {
		t.Fatalf("empty log: want 0 got %d", got)
	}

	// Append below capacity: Count tracks N exactly.
	for i := 1; i <= 5; i++ {
		l.Append(EventEntry{Time: int64(i), Type: "text"})
		if got := l.Count(); got != i {
			t.Fatalf("after %d appends: want %d got %d", i, i, got)
		}
	}

	// Append past capacity: Count caps at maxSize (count is monotonic up to
	// maxSize, never decremented on ring eviction).
	for i := 6; i <= 12; i++ {
		l.Append(EventEntry{Time: int64(i), Type: "text"})
	}
	if got := l.Count(); got != 8 {
		t.Fatalf("after overwrite: want 8 got %d", got)
	}

	// Count() must agree with the locked `count` field.
	l.mu.RLock()
	locked := l.count
	l.mu.RUnlock()
	if got := l.Count(); got != locked {
		t.Fatalf("atomic mirror diverged: Count()=%d count=%d", got, locked)
	}
}

func TestEventLog_Count_ConcurrentAppendRead(t *testing.T) {
	t.Parallel()

	const maxSize = 64
	const appends = 500
	l := NewEventLog(maxSize)

	var wg sync.WaitGroup
	wg.Add(2)

	// Writer.
	go func() {
		defer wg.Done()
		for i := 0; i < appends; i++ {
			l.Append(EventEntry{Time: int64(i), Type: "text"})
		}
	}()

	// Reader: races Count() against the writer; must never panic and must
	// stay within [0, maxSize].
	go func() {
		defer wg.Done()
		for i := 0; i < appends; i++ {
			if c := l.Count(); c < 0 || c > maxSize {
				t.Errorf("Count() out of range: %d", c)
				return
			}
		}
	}()

	wg.Wait()

	if got := l.Count(); got != maxSize {
		t.Fatalf("final Count(): want %d got %d", maxSize, got)
	}
}
