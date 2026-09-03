package cli

import (
	"sync"
	"testing"
)

// TestProcess_MeteringGen_TracksMeteringWrites pins the MeteringGen contract
// that ManagedSession's Snapshot cache keys on (#2345): zero until the first
// metering row lands, advances by exactly one per metadata frame that carries
// metering, and is untouched by frames that carry none.
func TestProcess_MeteringGen_TracksMeteringWrites(t *testing.T) {
	t.Parallel()
	p := &Process{}
	if got := p.MeteringGen(); got != 0 {
		t.Fatalf("zero-value MeteringGen = %d, want 0", got)
	}
	p.applyMetadata(&EventMetadata{ContextUsagePercent: 10})
	if got := p.MeteringGen(); got != 0 {
		t.Errorf("metadata frame without metering bumped MeteringGen to %d, want 0", got)
	}
	p.applyMetadata(&EventMetadata{MeteringUsage: []MeteringEntry{{Value: 0.01, Unit: "credit"}}})
	if got := p.MeteringGen(); got != 1 {
		t.Errorf("after first metering frame MeteringGen = %d, want 1", got)
	}
	// Same unit merges into the existing row (no new entry) but is still a
	// value change the cache must observe.
	p.applyMetadata(&EventMetadata{MeteringUsage: []MeteringEntry{{Value: 0.02, Unit: "credit"}}})
	if got := p.MeteringGen(); got != 2 {
		t.Errorf("after second metering frame MeteringGen = %d, want 2", got)
	}
	if usage := p.MeteringUsage(); len(usage) != 1 || usage[0].Value != 0.03 {
		t.Errorf("MeteringUsage = %+v, want single merged credit row 0.03", usage)
	}
}

// TestProcess_MeteringGen_ConcurrentReadersAndWriter exercises the
// gen/rows pair under -race: readers observe MeteringGen and MeteringUsage
// while a writer keeps appending. A reader that samples the gen BEFORE
// copying the rows must never see rows older than that gen (the invariant
// ManagedSession's cache relies on).
func TestProcess_MeteringGen_ConcurrentReadersAndWriter(t *testing.T) {
	t.Parallel()
	p := &Process{}
	const writes = 500
	const readers = 4
	var wg sync.WaitGroup
	wg.Add(1 + readers)
	go func() {
		defer wg.Done()
		for i := 0; i < writes; i++ {
			p.applyMetadata(&EventMetadata{MeteringUsage: []MeteringEntry{{Value: 1, Unit: "credit"}}})
		}
	}()
	errs := make(chan string, readers)
	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			for i := 0; i < writes; i++ {
				gen := p.MeteringGen()
				usage := p.MeteringUsage()
				// Rows may be NEWER than the sampled gen (a write landed
				// between the two reads) — that is the tolerated direction.
				// gen 0 therefore says nothing about the rows; only gen > 0
				// pins a lower bound on what the copy must contain.
				if gen == 0 {
					continue
				}
				if len(usage) != 1 || usage[0].Value < float64(gen) {
					errs <- "rows older than the gen sampled before the copy"
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if got := p.MeteringGen(); got != writes {
		t.Errorf("final MeteringGen = %d, want %d", got, writes)
	}
}
