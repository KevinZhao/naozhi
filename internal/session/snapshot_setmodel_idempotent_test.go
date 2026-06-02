package session

import (
	"testing"
)

// TestSnapshot_SetModel_NoStoreWhenUnchanged locks the R236-PERF-13 (#534)
// fix: Snapshot's "mirror live model" side effect must short-circuit when
// the cached value already equals proc.Model(). The dashboard polls
// Snapshot at 1Hz × N tabs × M sessions; an unconditional storeAtomicString
// call there dirties the model atomic.Pointer's cache line on every read.
//
// We assert by capturing the *string pointer stored in s.model after the
// first Snapshot (which is the legitimate first store) and verifying a
// subsequent Snapshot with an unchanged proc.Model() leaves that pointer
// identity untouched. A naive unconditional store would swap in a fresh
// *string each call.
func TestSnapshot_SetModel_NoStoreWhenUnchanged(t *testing.T) {
	t.Parallel()

	s := &ManagedSession{key: "test:direct:alice:general"}
	proc := NewTestProcess()
	proc.ModelVal = "claude-3-5-sonnet"
	s.storeProcess(proc)

	// First Snapshot legitimately stores the model.
	if got := s.Snapshot().Model; got != "claude-3-5-sonnet" {
		t.Fatalf("first Snapshot.Model = %q, want claude-3-5-sonnet", got)
	}

	first := s.model.Load()
	if first == nil {
		t.Fatal("model pointer nil after first Snapshot — initial store missing")
	}

	// Hammer Snapshot 64 times with the same proc.Model(). The pointer
	// MUST stay identical — every iteration that swaps in a fresh
	// *string is one wasted atomic store on the dashboard hot path.
	for i := 0; i < 64; i++ {
		s.Snapshot()
		if got := s.model.Load(); got != first {
			t.Fatalf("iter %d: model pointer swapped (%p -> %p) despite unchanged proc.Model() — Snapshot must not call SetModel when value matches",
				i, first, got)
		}
	}

	// Sanity: when proc.Model() actually changes, the mirror still fires.
	proc.ModelVal = "claude-3-5-haiku"
	if got := s.Snapshot().Model; got != "claude-3-5-haiku" {
		t.Fatalf("changed Snapshot.Model = %q, want claude-3-5-haiku", got)
	}
	if got := s.model.Load(); got == first {
		t.Fatal("model pointer unchanged after proc.Model() flipped — mirror failed to propagate live update")
	}
}

// TestSnapshotReadOnly_NoSetModelWrite locks R20260602-PERF-3 (#1577):
// the read-only snapshot variant (used by VisitSessions under r.mu.RLock)
// must NEVER call SetModel, even when the live proc.Model() disagrees with
// the persisted value. It still surfaces the live model in the returned
// snapshot — it just doesn't dirty the model atomic.Pointer on the read
// path.
func TestSnapshotReadOnly_NoSetModelWrite(t *testing.T) {
	t.Parallel()

	s := &ManagedSession{key: "test:direct:bob:general"}
	proc := NewTestProcess()
	proc.ModelVal = "claude-3-5-sonnet"
	s.storeProcess(proc)

	// Persisted model starts empty; the live proc reports a real value.
	// A mirroring Snapshot WOULD store it. snapshotReadOnly must not.
	before := s.model.Load() // expected nil
	if before != nil {
		t.Fatalf("precondition: persisted model = %v, want nil", before)
	}

	// Read-only snapshot still resolves the live model into the view...
	if got := s.snapshotReadOnly().Model; got != "claude-3-5-sonnet" {
		t.Fatalf("snapshotReadOnly().Model = %q, want claude-3-5-sonnet", got)
	}

	// ...but must leave the persisted atomic untouched (still nil): no
	// SetModel write happened on the read path.
	if after := s.model.Load(); after != nil {
		t.Fatalf("snapshotReadOnly mutated persisted model (%v); read path must be side-effect free", *after)
	}

	// Hammer it: 64 read-only snapshots, persisted model stays nil even
	// though live model differs every iteration.
	for i := 0; i < 64; i++ {
		_ = s.snapshotReadOnly()
		if after := s.model.Load(); after != nil {
			t.Fatalf("iter %d: snapshotReadOnly wrote model %v — must not mirror", i, *after)
		}
	}

	// Sanity: the mirroring Snapshot() still DOES persist (so the live
	// model reaches sessions.json via the dashboard poll path — Round 5
	// R5-3 is not regressed).
	if got := s.Snapshot().Model; got != "claude-3-5-sonnet" {
		t.Fatalf("Snapshot().Model = %q, want claude-3-5-sonnet", got)
	}
	if after := s.model.Load(); after == nil || *after != "claude-3-5-sonnet" {
		t.Fatal("Snapshot() failed to mirror live model into persisted field — R5-3 path regressed")
	}
}
