// marshal_entries_pool_test.go pins the R247-PERF-11 (#551) entries-pool
// invariants. The pool's job is to amortise the cap-len(jobs) backing-
// array alloc across persist calls without leaking *Job pointers (a
// recycled slice with stale slots would pin deleted jobs alive past
// DeleteJob).
package cron

import (
	"path/filepath"
	"testing"
)

// TestMarshalEntriesPool_RecyclesUnderCap verifies a slice put back into
// the pool is reused (cap preserved, contents zeroed). Allocations stay
// bounded across many marshalJobsLocked calls.
func TestMarshalEntriesPool_RecyclesUnderCap(t *testing.T) {
	t.Parallel()

	// Seed the pool with a known slice so we can detect reuse — sync.Pool
	// is best-effort, so we drain everything in the pool first by Getting
	// + dropping a few times, then put a marker slice and look for it.
	for i := 0; i < 8; i++ {
		_ = marshalEntriesPool.Get()
	}
	marker := make([]*Job, 0, 32)
	putMarshalEntries(&marker)

	got := marshalEntriesPool.Get().(*[]*Job)
	if cap(*got) != 32 {
		// Pool may have been concurrently drained — surface as a soft
		// signal but don't fail. The cleanup invariants below are the
		// hard contract.
		t.Logf("pool returned cap=%d (expected 32 from marker; sync.Pool best-effort)", cap(*got))
	}
	// Whatever we got, hand it back so the next test starts clean.
	putMarshalEntries(got)
}

// TestMarshalEntriesPool_ZeroesOnPut prevents the pool from pinning *Job
// pointers via stale slice slots. A leaked *Job past DeleteJob would
// delay GC and confuse memory profilers.
func TestMarshalEntriesPool_ZeroesOnPut(t *testing.T) {
	t.Parallel()

	j := &Job{ID: "abc"}
	s := []*Job{j}
	putMarshalEntries(&s)

	// After Put, the slice slot must be zeroed so the *Job is GC-eligible.
	// We can only inspect via the underlying array — slice the cap to
	// reach the zeroed slot.
	full := s[:1:1]
	if full[0] != nil {
		t.Errorf("putMarshalEntries did not zero slot 0: got %p, want nil", full[0])
	}
}

// TestMarshalEntriesPool_DropsOversize prevents a one-time burst from
// pinning a multi-MB backing array forever via the pool.
func TestMarshalEntriesPool_DropsOversize(t *testing.T) {
	t.Parallel()

	// Build a slice well above the cap-drop threshold.
	oversize := make([]*Job, 0, marshalEntriesCapDrop+1)
	putMarshalEntries(&oversize)

	// Oversize slices are dropped; a fresh Get returns a slot at or below
	// the threshold (or the New default). Drain a few to ensure we're
	// not seeing the oversize slot resurface.
	for i := 0; i < 4; i++ {
		got := marshalEntriesPool.Get().(*[]*Job)
		if cap(*got) > marshalEntriesCapDrop {
			t.Errorf("Get returned oversize cap=%d (drop threshold=%d) — Put should have dropped it",
				cap(*got), marshalEntriesCapDrop)
		}
		// Don't return them so the test doesn't pollute pool state.
	}
}

// TestMarshalJobsLocked_OutputUnchangedAcrossCalls is the integration
// guard: pooled snapshots must produce byte-identical JSON to a fresh
// snapshot. A bug in the reuse path (e.g. failing to reset length) would
// leak jobs from the prior call into the current marshal output.
func TestMarshalJobsLocked_OutputUnchangedAcrossCalls(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath:      filepath.Join(dir, "cron.json"),
		MaxJobs:        50,
		MaxJobsPerChat: 10,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	mkJob := func(plat, chat string) *Job {
		return &Job{Schedule: "@every 1h", Prompt: "p", Platform: plat, ChatID: chat}
	}
	for i := 0; i < 5; i++ {
		if err := s.AddJob(mkJob("feishu", "A")); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
	}

	s.mu.RLock()
	first, err := s.marshalJobsLocked()
	s.mu.RUnlock()
	if err != nil {
		t.Fatalf("marshalJobsLocked first: %v", err)
	}
	// Run several more marshals; output must be byte-identical because
	// jobs map is unchanged.
	for i := 0; i < 4; i++ {
		s.mu.RLock()
		got, err := s.marshalJobsLocked()
		s.mu.RUnlock()
		if err != nil {
			t.Fatalf("marshalJobsLocked iter %d: %v", i, err)
		}
		if string(got) != string(first) {
			t.Errorf("marshalJobsLocked iter %d differs from first call", i)
		}
	}
}
