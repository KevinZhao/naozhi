package cli

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestProcess_ReadsUnderRLock_AllowConcurrency verifies that R70-PERF-L3's
// switch of Process.mu from sync.Mutex to sync.RWMutex lets many concurrent
// GetState / IsRunning / GetSessionID / TotalCost readers proceed in parallel.
//
// With a Mutex these four goroutines would serialise through RLock slots —
// here we hold one RLock open (via a sentinel goroutine) and confirm the
// remaining readers do not block on it. The test fails on the regression
// where a future refactor flips mu back to sync.Mutex.
func TestProcess_ReadsUnderRLock_AllowConcurrency(t *testing.T) {
	p := &Process{State: StateRunning, SessionID: "sess"}

	// Grab and hold an RLock in a helper goroutine so the readers below can
	// only make progress if they also go through RLock (shared mode). If any
	// of them took the write lock we would deadlock.
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		p.mu.RLock()
		close(held)
		<-release
		p.mu.RUnlock()
	}()
	<-held
	defer close(release)

	const readers = 32
	var wg sync.WaitGroup
	var done atomic.Int32
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			_ = p.GetState()
			_ = p.IsRunning()
			_ = p.GetSessionID()
			_ = p.TotalCost()
			done.Add(1)
		}()
	}

	// Expect every reader to complete promptly even while the sentinel RLock
	// is held. 2s is generous; real completion is sub-millisecond.
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for done.Load() < readers {
		select {
		case <-deadline:
			t.Fatalf("only %d/%d concurrent readers completed within 2s; mu is not RWMutex?", done.Load(), readers)
		case <-ticker.C:
		}
	}
	wg.Wait()
}
