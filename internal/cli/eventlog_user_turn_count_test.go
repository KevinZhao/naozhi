package cli

import (
	"sync"
	"testing"
)

// TestEventLog_UserTurnCount_Append locks the per-Append increment contract.
// A user entry bumps the counter by exactly one; non-user entry types leave
// it untouched. This is the core invariant SessionSnapshot.MessageCount
// depends on — regressions here silently change the sidebar display.
func TestEventLog_UserTurnCount_Append(t *testing.T) {
	t.Parallel()

	l := NewEventLog(10)
	if got := l.UserTurnCount(); got != 0 {
		t.Fatalf("initial count = %d, want 0", got)
	}

	// Non-user entries must NOT increment.
	l.Append(EventEntry{Type: "system", Summary: "boot"})
	l.Append(EventEntry{Type: "thinking", Summary: "..."})
	l.Append(EventEntry{Type: "tool_use", Summary: "Read"})
	l.Append(EventEntry{Type: "result", Summary: "done"})
	if got := l.UserTurnCount(); got != 0 {
		t.Errorf("after non-user appends count = %d, want 0", got)
	}

	// Each user entry bumps by one.
	l.Append(EventEntry{Type: "user", Summary: "hi"})
	if got := l.UserTurnCount(); got != 1 {
		t.Errorf("after 1 user = %d, want 1", got)
	}
	l.Append(EventEntry{Type: "user", Summary: "again"})
	if got := l.UserTurnCount(); got != 2 {
		t.Errorf("after 2 user = %d, want 2", got)
	}
}

// TestEventLog_UserTurnCount_AppendBatch locks the batch-merge contract:
// a single AppendBatch of N user entries records a single atomic.Add(N)
// rather than N separate increments. Both are functionally equivalent for
// callers, but the batched form matches the lastPromptSummary single-store
// pattern so concurrent Snapshot observes the batch as one event.
func TestEventLog_UserTurnCount_AppendBatch(t *testing.T) {
	t.Parallel()

	l := NewEventLog(20)
	entries := []EventEntry{
		{Type: "user", Summary: "first"},
		{Type: "tool_use", Summary: "Read"},
		{Type: "thinking", Summary: "..."},
		{Type: "user", Summary: "second"},
		{Type: "result", Summary: "done"},
		{Type: "user", Summary: "third"},
	}
	l.AppendBatch(entries)

	if got := l.UserTurnCount(); got != 3 {
		t.Errorf("after AppendBatch with 3 user entries count = %d, want 3", got)
	}

	// Empty batch is a no-op and must not touch the counter.
	l.AppendBatch(nil)
	l.AppendBatch([]EventEntry{})
	if got := l.UserTurnCount(); got != 3 {
		t.Errorf("after empty batches count = %d, want 3", got)
	}

	// Batch with no user entries also leaves the counter alone.
	l.AppendBatch([]EventEntry{
		{Type: "tool_use", Summary: "Write"},
		{Type: "result", Summary: "done"},
	})
	if got := l.UserTurnCount(); got != 3 {
		t.Errorf("after non-user batch count = %d, want 3", got)
	}
}

// TestEventLog_UserTurnCount_SurvivesRingEviction pins the "cumulative"
// semantic: the counter does not decrement when the ring buffer evicts old
// entries. Dashboard surfaces the counter as "conversation turns so far",
// not as "live entries in the log", so eviction must not roll it back.
func TestEventLog_UserTurnCount_SurvivesRingEviction(t *testing.T) {
	t.Parallel()

	// Tiny ring buffer: entries beyond maxSize get evicted.
	l := NewEventLog(3)
	for i := 0; i < 10; i++ {
		l.Append(EventEntry{Type: "user", Summary: "msg"})
	}
	if got := l.UserTurnCount(); got != 10 {
		t.Errorf("count = %d, want 10 (eviction must not decrement)", got)
	}
	// Entries() reflects eviction (only 3 entries in the ring), confirming
	// the test actually exercised the overwrite path.
	if n := len(l.Entries()); n != 3 {
		t.Errorf("ring size = %d, want 3 (test setup sanity check)", n)
	}
}

// TestEventLog_UserTurnCount_ConcurrentAppends runs many goroutines
// bumping the counter via Append. With -race any torn read/write would
// surface. Final count must equal goroutines × per-goroutine iterations
// exactly — atomic.Add contract, not an approximation.
func TestEventLog_UserTurnCount_ConcurrentAppends(t *testing.T) {
	t.Parallel()

	l := NewEventLog(0) // default size, plenty of room
	const goroutines = 16
	const perG = 100

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				l.Append(EventEntry{Type: "user", Summary: "concurrent"})
			}
		}()
	}
	wg.Wait()

	want := int64(goroutines * perG)
	if got := l.UserTurnCount(); got != want {
		t.Errorf("concurrent count = %d, want %d", got, want)
	}
}
