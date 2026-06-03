package persist

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// TestFlushAllLocked_ReusesScratchSlices pins R20260603030037-PERF-9:
// flushAllLocked must reuse the p.flushAllKeys/p.flushAllWs scratch slices
// across opFlushAll calls instead of allocating two fresh slices each time
// (mirrors tickFlush's tickFlushKeys/tickFlushWs reuse). The functional
// contract — every dirty writer is durably flushed by an explicit Flush —
// must stay intact, and the writer-pointer scratch must be cleared after
// each call so dropped/idle writers can be GC'd.
//
// Flush blocks until the run goroutine finishes the op, and no other op is
// in flight while the test inspects the scratch fields, so reading them from
// the test goroutine after Flush returns observes a settled state without a
// data race.
func TestFlushAllLocked_ReusesScratchSlices(t *testing.T) {
	p, dir := newTestPersister(t, func(o *Options) {
		// Long flush interval so tickFlush does not race the explicit
		// Flush calls — we want the opFlushAll path exercised directly.
		o.FlushInterval = time.Hour
	})

	ctx := context.Background()

	writeKeys := func(prefix string, n int) {
		for i := 0; i < n; i++ {
			key := fmt.Sprintf("%s-%02d", prefix, i)
			sink := p.SinkFor(key)
			sink([]Entry{entry(t, int64(1700000000000+i), fmt.Sprintf("u%d", i))}, false)
		}
	}

	assertPersisted := func(prefix string, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			key := fmt.Sprintf("%s-%02d", prefix, i)
			idx, err := ReadAllIdx(filepath.Join(dir, KeyHash(key)+idxExt))
			if err != nil {
				t.Fatalf("ReadAllIdx(%s): %v", key, err)
			}
			if len(idx) == 0 {
				t.Errorf("key %s has no idx entries after Flush", key)
			}
		}
	}

	// First Flush over a small dirty set.
	const n1 = 6
	writeKeys("a", n1)
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("first Flush: %v", err)
	}
	assertPersisted("a", n1)

	capKeys1 := cap(p.flushAllKeys)
	capWs1 := cap(p.flushAllWs)
	if capKeys1 == 0 || capWs1 == 0 {
		t.Fatalf("scratch slices not populated after first Flush: capKeys=%d capWs=%d", capKeys1, capWs1)
	}
	// After Flush the len is reset to the dirty count, but the writer-pointer
	// backing array must be cleared so writers can be GC'd.
	for i, w := range p.flushAllWs[:cap(p.flushAllWs)] {
		if w != nil {
			t.Fatalf("flushAllWs[%d] not cleared after Flush: %v", i, w)
		}
	}

	// A second Flush with no new dirty writers must reuse the same backing
	// arrays (no growth, no fresh allocation).
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("second (no-op) Flush: %v", err)
	}
	if cap(p.flushAllKeys) != capKeys1 || cap(p.flushAllWs) != capWs1 {
		t.Errorf("scratch capacity changed on no-op Flush: keys %d->%d ws %d->%d",
			capKeys1, cap(p.flushAllKeys), capWs1, cap(p.flushAllWs))
	}

	// A larger dirty set may grow the scratch; the grown backing array must
	// persist (be written back), and the functional contract must hold.
	const n2 = 40 // > parallelFsyncMaxWorkers(8) so multiple worker batches run
	writeKeys("b", n2)
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("third Flush: %v", err)
	}
	assertPersisted("b", n2)
	if cap(p.flushAllKeys) < n2 || cap(p.flushAllWs) < n2 {
		t.Errorf("grown scratch not written back: capKeys=%d capWs=%d want >= %d",
			cap(p.flushAllKeys), cap(p.flushAllWs), n2)
	}
	for i, w := range p.flushAllWs[:cap(p.flushAllWs)] {
		if w != nil {
			t.Fatalf("flushAllWs[%d] not cleared after grown Flush: %v", i, w)
		}
	}
}
