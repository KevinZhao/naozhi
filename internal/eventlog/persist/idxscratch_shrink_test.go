package persist

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// TestPerKeyWriter_IdxScratchShrinksAfterLargeBatch pins R250-PERF-17
// (#1120): after a one-off InjectHistory-sized batch inflates a writer's
// idxScratch backing array beyond IdxStride*4, the next steady-state
// flush must release the oversized scratch so 100+ active writers do
// not each hold a multi-KiB pinned slice for the writer's lifetime.
//
// Flush only paths through the shrink branch when entries actually
// survive selectForIdx (stride > 1 + non-empty pendingIdx), so the
// fixture pushes through a Persister at IdxStride=4 to make the
// shrink fire on a realistic per-key writer.
func TestPerKeyWriter_IdxScratchShrinksAfterLargeBatch(t *testing.T) {
	p, _ := newTestPersister(t, func(o *Options) {
		// Match newTestPersister default but make the contract explicit.
		o.IdxStride = 4
	})

	const key = "dashboard:direct:alice:bigbatch"
	sink := p.SinkFor(key)

	// First batch: 2000 entries — well above IdxStride*4=16. selectForIdx
	// keeps roughly len/stride entries (plus header + last), so the
	// resulting kept slice (which becomes idxScratch) bloats far past the
	// steady-state target of IdxStride*2=8.
	const bigN = 2000
	big := make([]Entry, 0, bigN)
	base := int64(1_700_000_000_000)
	for i := 0; i < bigN; i++ {
		big = append(big, entry(t, base+int64(i), "uuid-big"))
	}
	sink(big, false /* replay */)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush after big batch: %v", err)
	}

	// At this point idxScratch retains the inflated cap from the big batch.
	// A second small batch + flush should trip the shrink-rule on the
	// previous flush's idxScratch and bring cap back to IdxStride*2.
	sink([]Entry{entry(t, base+int64(bigN+1), "uuid-small")}, false)
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush after small batch: %v", err)
	}

	// p.writers is owned by the run goroutine and accessed without a
	// lock. After a synchronous Flush() returns, the run goroutine has
	// re-parked on the select and no other goroutine in this test mutates
	// the map (the t.Cleanup Stop has not fired yet). Reading the map
	// here is safe — same pattern as TestCollectFlushCandidates_*.
	w := p.writers[key]
	if w == nil {
		t.Fatalf("writer for %q missing after flush", key)
	}

	threshold := p.opts.IdxStride * 4
	if cap(w.idxScratch) > threshold {
		t.Errorf("idxScratch cap %d still exceeds shrink threshold %d after small follow-up flush — pendingIdx-shrink rule did not propagate to idxScratch (R250-PERF-17 #1120)", cap(w.idxScratch), threshold)
	}
	// pendingIdx is the existing reset rule — sanity-check it stayed shrunk
	// so we know IdxStride*2 sizing is the post-fix target for both slices.
	if cap(w.pendingIdx) > threshold {
		t.Errorf("pendingIdx cap %d > %d after follow-up small batch", cap(w.pendingIdx), threshold)
	}
}

// silence unused-import warnings if schema becomes unused after refactors.
var _ = schema.TypeEntry
