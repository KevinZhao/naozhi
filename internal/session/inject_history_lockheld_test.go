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

	// Wait until the reader has actually been scheduled at least once so the
	// baseline is meaningful even on a busy CI box (a fixed sleep can elapse
	// before the goroutine first runs).
	startIters := waitForReaderProgress(t, &readerIters, 0)

	s.InjectHistory(entries)

	// The reader doesn't block on InjectHistory's lock-held phase under the
	// optimisation, so it must make further progress after the call returns.
	// Poll with a generous timeout instead of sampling a fixed window: this
	// preserves the "not stalled by the lock-held copy" assertion without
	// assuming the reader goroutine gets scheduled within a few ms (the source
	// of R237-PERF-6's flakiness under CI scheduling variance).
	progress := waitForReaderProgress(t, &readerIters, startIters) - startIters
	close(stop)
	wg.Wait()

	if progress < 1 {
		t.Errorf("reader made %d iterations during/after InjectHistory; want ≥1 (lock-held copy would have stalled it)", progress)
	}
}

// waitForReaderProgress polls counter until it exceeds baseline, returning the
// observed value. It fails the test if the counter never advances within a
// generous timeout — i.e. the reader is genuinely stalled (a real regression),
// not merely slow to be scheduled.
func waitForReaderProgress(t *testing.T, counter *atomic.Int64, baseline int64) int64 {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v := counter.Load(); v > baseline {
			return v
		}
		time.Sleep(200 * time.Microsecond)
	}
	t.Fatalf("reader made no progress past %d within timeout; appears genuinely stalled", baseline)
	return 0
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
