package subagent

import (
	"sync"
	"testing"
	"time"
)

// TestScanMetaFiles_IOOutsideWriteLock pins [R164029-PERF-3] (#1595):
// rawScanSubagentsDir's disk IO must run WITHOUT l.mu held, so a concurrent
// Resolve fast-path (which takes only an RLock for its cache double-check) is
// not stalled behind a slow directory scan. We block inside the scanHook
// (which now fires before the write lock is taken) and assert a goroutine that
// grabs l.mu.RLock proceeds while the scan is parked.
func TestScanMetaFiles_IOOutsideWriteLock(t *testing.T) {
	t.Parallel()
	const sessionID = "abcdef01-2345-6789-abcd-ef0123456788"
	l, subagentDir := newLinkerForTest(t, sessionID)
	l.cacheTTL = time.Hour // cold cache stays cold for the whole test

	now := time.Now()
	writeAgentFiles(t, subagentDir, "12121212121212121", "io-worker", sessionID, "p1", now)

	scanEntered := make(chan struct{})
	releaseScan := make(chan struct{})
	l.scanHook = func() {
		close(scanEntered)
		<-releaseScan // park the scan; if it held l.mu, the RLock below deadlocks
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		l.scanMetaFiles(subagentDir)
	}()

	<-scanEntered
	// While the scan is parked, an RLock acquirer must NOT block.
	gotLock := make(chan struct{})
	go func() {
		l.mu.RLock()
		//lint:ignore SA2001 intentional empty critical section: probes that the lock is acquirable
		l.mu.RUnlock()
		close(gotLock)
	}()
	select {
	case <-gotLock:
		// good: scan does not hold l.mu during IO
	case <-time.After(2 * time.Second):
		close(releaseScan)
		t.Fatal("RLock blocked while scan parked — scan still holds l.mu during IO")
	}
	close(releaseScan)
	wg.Wait()
}
