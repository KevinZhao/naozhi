package session

// R215-ARCH-P1-2 regression tests. spawnSession used to manage
// r.pendingSpawns via 4 manually-paired ++ / -- segments. panicSafeSpawn
// covered only one of them. Any panic in the other 3 (or any future
// early-return added without a manual --) would strand the counter and
// every subsequent GetOrCreate would refuse with ErrMaxProcs until the
// process restarted.
//
// The fix wraps the increment in an RAII slot token whose release is
// idempotent: explicit happy-path callers still decrement at the original
// site (preserving the existing lock-state contract for the second
// `pendingSpawns--`, which happens after the post-Spawn re-lock), and a
// `defer slot.release()` absorbs any unexpected exit. These tests pin
// that behaviour against a minimal Router so a future refactor that
// removes the defer will fail the panic-path assertion.

import (
	"testing"
)

// TestPendingSpawnSlot_ReleaseLockedIsIdempotent: explicit happy-path
// release runs once; the deferred release() must be a no-op.
func TestPendingSpawnSlot_ReleaseLockedIsIdempotent(t *testing.T) {
	t.Parallel()

	r := &Router{}
	r.mu.Lock()
	slot := r.acquirePendingSpawnSlotLocked()
	if r.pendingSpawns != 1 {
		t.Fatalf("pendingSpawns=%d after acquire, want 1", r.pendingSpawns)
	}
	slot.releaseLocked()
	if r.pendingSpawns != 0 {
		t.Fatalf("pendingSpawns=%d after releaseLocked, want 0", r.pendingSpawns)
	}
	r.mu.Unlock()

	// Defer-style release must be a no-op (idempotent).
	slot.release()
	r.mu.Lock()
	if r.pendingSpawns != 0 {
		t.Fatalf("pendingSpawns=%d after redundant release, want 0 (idempotent)", r.pendingSpawns)
	}
	r.mu.Unlock()
}

// TestPendingSpawnSlot_DeferReleaseAbsorbsPanic: the defer must decrement
// pendingSpawns when a panic prevents the explicit releaseLocked() call.
// This is the core R215-ARCH-P1-2 guard: future code added between
// ++ and -- that panics MUST NOT strand the counter.
func TestPendingSpawnSlot_DeferReleaseAbsorbsPanic(t *testing.T) {
	t.Parallel()

	r := &Router{}

	func() {
		defer func() {
			// We expect a panic — recover and continue. The slot's defer
			// fires BEFORE this recover (LIFO defer order), so by the
			// time we land here pendingSpawns is already back to 0.
			_ = recover()
		}()

		r.mu.Lock()
		slot := r.acquirePendingSpawnSlotLocked()
		defer slot.release()
		r.mu.Unlock()

		// Simulate a panic between ++ and the matching -- (e.g., the
		// "future refactor introduces panic in the other 3 segments"
		// scenario from the issue).
		panic("synthetic panic between ++ and --")
	}()

	r.mu.Lock()
	got := r.pendingSpawns
	r.mu.Unlock()
	if got != 0 {
		t.Fatalf("pendingSpawns=%d after panic-then-defer-release, want 0 (counter would otherwise strand permanently and every GetOrCreate would refuse with ErrMaxProcs until restart)", got)
	}
}

// TestPendingSpawnSlot_ReleaseTakesLock: when releaseLocked was never
// called, release() must acquire r.mu itself and decrement.
func TestPendingSpawnSlot_ReleaseTakesLock(t *testing.T) {
	t.Parallel()

	r := &Router{}
	r.mu.Lock()
	slot := r.acquirePendingSpawnSlotLocked()
	r.mu.Unlock()

	slot.release() // must self-lock + decrement

	r.mu.Lock()
	got := r.pendingSpawns
	r.mu.Unlock()
	if got != 0 {
		t.Fatalf("pendingSpawns=%d after release(), want 0", got)
	}
}

// TestPendingSpawnSlot_NilReleaseSafe: release on a nil receiver must be
// a no-op so a defer that captured a nil slot (e.g., acquire path errored
// before assignment) does not crash.
func TestPendingSpawnSlot_NilReleaseSafe(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil release panicked: %v", r)
		}
	}()
	var slot *pendingSpawnSlot
	slot.release()
	slot.releaseLocked()
}

// TestPendingSpawnSlot_DoubleReleasePathsAreSafe: spawnSession's happy
// path calls releaseLocked() inline and the function-level defer also
// fires release(). Both must net to a single decrement (single
// goroutine — spawnSession is the only owner of the slot, so this is
// not a concurrent test, just a sequential idempotency pin matching
// the actual production call shape).
func TestPendingSpawnSlot_DoubleReleasePathsAreSafe(t *testing.T) {
	t.Parallel()

	r := &Router{}
	r.mu.Lock()
	slot := r.acquirePendingSpawnSlotLocked()
	r.mu.Unlock()

	// First: simulate the post-Spawn re-lock + releaseLocked happy path.
	r.mu.Lock()
	slot.releaseLocked()
	r.mu.Unlock()

	// Then: the deferred release() at function exit must be a no-op.
	slot.release()

	r.mu.Lock()
	got := r.pendingSpawns
	r.mu.Unlock()
	if got != 0 {
		t.Fatalf("pendingSpawns=%d after happy-path releaseLocked + defer release, want 0", got)
	}
}
