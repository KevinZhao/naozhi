package persist

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// TestTickFlush_ParallelPersistsAllDirtyWriters pins R20260602-091302-PERF-3
// (#1569): the debounced flush tick fans every dirty writer's fsync over the
// bounded worker pool (same as flushAllLocked/shutdownAll) instead of a serial
// loop. Functionally the contract is unchanged — every dirty writer must still
// be durably flushed by the tick — so this test writes to many distinct keys,
// lets the run loop's flush ticker fire (NOT an explicit Flush, which would
// bypass tickFlush), and asserts every key landed an idx entry.
func TestTickFlush_ParallelPersistsAllDirtyWriters(t *testing.T) {
	p, dir := newTestPersister(t, func(o *Options) {
		o.FlushInterval = 20 * time.Millisecond
	})

	const n = 24 // > parallelFsyncMaxWorkers(8) so multiple worker batches run
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("key-%02d", i)
		sink := p.SinkFor(key)
		sink([]Entry{entry(t, int64(1700000000000+i), fmt.Sprintf("u%d", i))}, false)
	}

	// Wait for the debounced flush ticker (flushTick = FlushInterval/2, floored
	// at 10ms) to fire tickFlush. Poll the idx files rather than a fixed sleep
	// so the test stays fast and non-flaky.
	deadline := time.Now().Add(3 * time.Second)
	for {
		missing := 0
		for i := 0; i < n; i++ {
			key := fmt.Sprintf("key-%02d", i)
			idx, err := ReadAllIdx(filepath.Join(dir, KeyHash(key)+idxExt))
			if err != nil || len(idx) == 0 {
				missing++
			}
		}
		if missing == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tickFlush did not persist all %d writers in time: %d still missing idx entries", n, missing)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Every key must have its entry recorded.
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("key-%02d", i)
		idx, err := ReadAllIdx(filepath.Join(dir, KeyHash(key)+idxExt))
		if err != nil {
			t.Fatalf("ReadAllIdx(%s): %v", key, err)
		}
		if len(idx) == 0 {
			t.Errorf("key %s has no idx entries after tickFlush", key)
		}
	}
}
