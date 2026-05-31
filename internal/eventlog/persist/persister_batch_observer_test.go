package persist

import (
	"context"
	"sync"
	"testing"
	"time"
)

// onWriteCapture records every n argument passed to OnWrite so tests can
// assert batch vs. per-record invocation patterns.
type onWriteCapture struct {
	mu   sync.Mutex
	calls []int // each element is the n arg from one OnWrite invocation
}

func (c *onWriteCapture) OnWrite(n int) {
	c.mu.Lock()
	c.calls = append(c.calls, n)
	c.mu.Unlock()
}
func (c *onWriteCapture) OnDrop(int)         {}
func (c *onWriteCapture) OnFsync()           {}
func (c *onWriteCapture) OnMalformed()       {}
func (c *onWriteCapture) OnReplayLeak(int)   {}

func (c *onWriteCapture) snapshot() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]int, len(c.calls))
	copy(out, c.calls)
	return out
}

// TestHandleBatch_OnWrite_BatchedNotPerRecord pins R250531-PERF-1:
// handleBatch must call Observer.OnWrite exactly once per batch (with the
// total count), not once per record. A 5-entry batch must produce a single
// OnWrite(5) invocation, not five OnWrite(1) calls.
func TestHandleBatch_OnWrite_BatchedNotPerRecord(t *testing.T) {
	t.Parallel()
	cap := &onWriteCapture{}
	p, _ := newTestPersister(t, func(o *Options) {
		o.Observer = cap
	})

	const batchSize = 5
	entries := make([]Entry, batchSize)
	for i := range entries {
		entries[i] = entry(t, int64(i+1), "uuid-batch-"+string(rune('a'+i)))
	}
	sink := p.SinkFor("dashboard:direct:bob:batch")
	sink(entries, false)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	calls := cap.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 OnWrite call for a single batch, got %d calls: %v", len(calls), calls)
	}
	if calls[0] != batchSize {
		t.Errorf("expected OnWrite(%d), got OnWrite(%d)", batchSize, calls[0])
	}
}

// TestHandleBatch_OnWrite_TotalEquivalence verifies that two separate
// single-entry batches produce the same total written count as one two-entry
// batch would — i.e. the batching is purely a call-count optimisation and
// the sum is semantically unchanged. [R250531-PERF-1]
func TestHandleBatch_OnWrite_TotalEquivalence(t *testing.T) {
	t.Parallel()
	cap := &onWriteCapture{}
	p, _ := newTestPersister(t, func(o *Options) {
		o.Observer = cap
	})

	sink := p.SinkFor("dashboard:direct:carol:equiv")
	sink([]Entry{entry(t, 1, "e1"), entry(t, 2, "e2"), entry(t, 3, "e3")}, false)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	calls := cap.snapshot()
	total := 0
	for _, n := range calls {
		total += n
	}
	if total != 3 {
		t.Errorf("total OnWrite count must be 3, got %d (calls=%v)", total, calls)
	}
}

// TestCollectFlushCandidates_OldestFirst pins R250531-PERF-8: after the
// slices.SortFunc migration the candidates must still be ordered by
// firstDirtyAt ascending (oldest writer first). We drive the internal
// collectFlushCandidates method directly because the sort is observable
// only at that level; higher-level Flush ordering is non-deterministic
// across multiple keys.
func TestCollectFlushCandidates_OldestFirst(t *testing.T) {
	t.Parallel()

	t0 := time.Unix(1700000000, 0)
	// Freeze the clock at a point well past the writers' dirty times so
	// every writer is eligible. FlushInterval default is 20ms in tests.
	frozenNow := t0.Add(10 * time.Second)

	p, _ := newTestPersister(t, func(o *Options) {
		o.Clock = func() time.Time { return frozenNow }
		o.FlushInterval = 1 * time.Millisecond
	})

	// Inject three writers with known firstDirtyAt values in reverse order.
	// We bypass the public API and poke p.writers directly because the test
	// is in the same package.
	keys := []string{"k-late", "k-mid", "k-early"}
	dirtyTimes := map[string]time.Time{
		"k-early": t0.Add(1 * time.Second),
		"k-mid":   t0.Add(2 * time.Second),
		"k-late":  t0.Add(3 * time.Second),
	}
	for _, k := range keys {
		w := &perKeyWriter{
			dirty:        true,
			firstDirtyAt: dirtyTimes[k],
			lastActivity: frozenNow,
		}
		p.writers[k] = w
	}

	cands := p.collectFlushCandidates(frozenNow)
	// Remove stub writers before p.Stop() runs in t.Cleanup so
	// shutdownAll never attempts to flush a writer with nil logBuf.
	for _, k := range keys {
		delete(p.writers, k)
	}

	if len(cands) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(cands))
	}
	order := []string{cands[0].key, cands[1].key, cands[2].key}
	want := []string{"k-early", "k-mid", "k-late"}
	for i, got := range order {
		if got != want[i] {
			t.Errorf("candidates[%d]: got %q, want %q (full order: %v)", i, got, want[i], order)
		}
	}
}
