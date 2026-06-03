package persist

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// TestPersister_FlushAll_ScratchReuse pins R20260603-PERF-2: flushAllLocked
// reuses run-goroutine-owned scratch slices (flushAllKeys/flushAllWs) via
// [:0] truncation. The reuse must not leak a writer from a prior Flush into a
// later one: each Flush must persist exactly the writers dirtied since the
// previous flush. We drive a wide first batch (grows the scratch), then a
// narrow second batch (must not re-flush already-clean writers from batch 1),
// then a wide third batch (scratch grows again from the truncated reuse).
func TestPersister_FlushAll_ScratchReuse(t *testing.T) {
	p, dir := newTestPersister(t)

	key := func(i int) string { return fmt.Sprintf("dashboard:direct:u%d:general", i) }

	flush := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := p.Flush(ctx); err != nil {
			t.Fatalf("Flush: %v", err)
		}
	}

	// Batch 1: 12 writers, each gets one entry. Grows scratch to >=12.
	for i := 0; i < 12; i++ {
		p.SinkFor(key(i))([]Entry{entry(t, 1700000000000+int64(i), fmt.Sprintf("b1-%d", i))}, false)
	}
	flush()

	// Batch 2: only writers 0 and 1 get a new entry. Scratch is reused
	// (truncated to [:0]); only 2 writers should be re-collected as dirty.
	for i := 0; i < 2; i++ {
		p.SinkFor(key(i))([]Entry{entry(t, 1700000100000+int64(i), fmt.Sprintf("b2-%d", i))}, false)
	}
	flush()

	// Batch 3: writers 0..7 each get another entry. Scratch grows again.
	for i := 0; i < 8; i++ {
		p.SinkFor(key(i))([]Entry{entry(t, 1700000200000+int64(i), fmt.Sprintf("b3-%d", i))}, false)
	}
	flush()

	// Verify per-writer record counts match the number of entries written.
	// header(1) + entries; writers 0,1 -> 3 entries; 2..7 -> 2; 8..11 -> 1.
	wantEntries := func(i int) int {
		switch {
		case i < 2:
			return 3
		case i < 8:
			return 2
		default:
			return 1
		}
	}
	for i := 0; i < 12; i++ {
		recs := readAllRecords(t, LogPath(dir, key(i)))
		entries := 0
		for _, r := range recs {
			if r.Type == schema.TypeEntry {
				entries++
			}
		}
		if entries != wantEntries(i) {
			t.Fatalf("writer %d: got %d entries on disk, want %d (scratch reuse leaked/dropped a flush)",
				i, entries, wantEntries(i))
		}
	}
}
