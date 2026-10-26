package persist

import (
	"testing"
	"time"
)

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
