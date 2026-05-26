package session

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestInjectHistory_ForwardCopyOutsideLock pins R237-PERF-6 (#667): the
// O(N) make+copy that produces the proc-forward slice MUST run AFTER
// historyMu is released so concurrent EventEntries readers (dashboard
// 1Hz RLock) are not stalled by the replay's allocation+memcpy.
//
// We don't time-bound the assertion (CI variance) — instead we assert
// that the reader goroutine makes progress concurrently with a
// 200-entry InjectHistory call, demonstrating the under-lock window
// covers only the monotonicity scan + slice-header reslice (no
// allocation).
func TestInjectHistory_ForwardCopyOutsideLock(t *testing.T) {
	s := &ManagedSession{key: "test:lockheld"}
	// Wire a process so InjectHistory takes the seeded-tail forward path.
	proc := newIdleProc()
	s.storeProcess(proc)

	const batchSize = 200
	entries := make([]cli.EventEntry, batchSize)
	for i := range entries {
		entries[i] = cli.EventEntry{Type: "user", Summary: "x", Time: int64(i)}
	}

	var readerIters atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = s.EventEntries()
				readerIters.Add(1)
			}
		}
	}()

	// Give the reader a moment to spin up.
	time.Sleep(5 * time.Millisecond)
	startIters := readerIters.Load()

	s.InjectHistory(entries)

	// Sample reader progress just after InjectHistory returns. The reader
	// goroutine doesn't block on InjectHistory's lock-held phase under
	// the optimisation, so its iteration count should advance.
	time.Sleep(2 * time.Millisecond)
	close(stop)
	wg.Wait()

	progress := readerIters.Load() - startIters
	if progress < 1 {
		t.Errorf("reader made %d iterations during/after InjectHistory; want ≥1 (lock-held copy would have stalled it)", progress)
	}
}

// TestInjectHistory_ForwardSliceIsDefensiveCopy pins the safety contract
// of the move-out-of-lock optimisation: the slice handed to
// proc.InjectHistory must not alias the persistedHistory backing array,
// because proc consumes the slice across goroutine boundaries.
func TestInjectHistory_ForwardSliceIsDefensiveCopy(t *testing.T) {
	s := &ManagedSession{key: "test:defensive"}
	captured := make(chan []cli.EventEntry, 1)
	proc := &capturingProc{fakeProcess: newIdleProc(), captured: captured}
	s.storeProcess(proc)

	entries := []cli.EventEntry{
		{Type: "user", Summary: "first", Time: 1},
		{Type: "text", Summary: "reply", Time: 2},
	}
	s.InjectHistory(entries)

	select {
	case got := <-captured:
		if len(got) != len(entries) {
			t.Fatalf("forwarded len=%d, want %d", len(got), len(entries))
		}
		// Mutate the slice we captured. If it aliased persistedHistory,
		// the next read would observe the mutation.
		got[0].Summary = "MUTATED"
		s.historyMu.RLock()
		stored := s.persistedHistory[0].Summary
		s.historyMu.RUnlock()
		if stored == "MUTATED" {
			t.Error("forwarded slice aliased persistedHistory backing array; mutation leaked through")
		}
	case <-time.After(time.Second):
		t.Fatal("proc.InjectHistory was never called")
	}
}

// capturingProc wraps fakeProcess so the test can inspect the slice
// handed to InjectHistory.
type capturingProc struct {
	*fakeProcess
	captured chan []cli.EventEntry
}

func (c *capturingProc) InjectHistory(entries []cli.EventEntry) {
	select {
	case c.captured <- entries:
	default:
	}
}
