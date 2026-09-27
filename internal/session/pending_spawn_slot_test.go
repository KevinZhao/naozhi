package session

// R215-ARCH-P1-2 regression tests. The spawn used to manage
// the pending-spawn count via 4 manually-paired ++ / -- segments. panicSafeSpawn
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

	r := &Router{ss: newSessionTable()}
	var slot pendingSpawnSlot
	r.ss.Update(func(tx sessTx) {
		slot = r.acquirePendingSpawnSlot(tx)
		if tx.Ext().spawns.PendingSpawns() != 1 {
			t.Fatalf("pendingSpawns=%d after acquire, want 1", tx.Ext().spawns.PendingSpawns())
		}
		slot.releaseIn(tx)
		if tx.Ext().spawns.PendingSpawns() != 0 {
			t.Fatalf("pendingSpawns=%d after releaseIn, want 0", tx.Ext().spawns.PendingSpawns())
		}
	})

	// Defer-style release must be a no-op (idempotent).
	slot.release()
	r.ss.Update(func(tx sessTx) {
		if tx.Ext().spawns.PendingSpawns() != 0 {
			t.Fatalf("pendingSpawns=%d after redundant release, want 0 (idempotent)", tx.Ext().spawns.PendingSpawns())
		}
	})
}

// TestPendingSpawnSlot_DeferReleaseAbsorbsPanic: the defer must decrement
// pendingSpawns when a panic prevents the explicit releaseIn() call.
// This is the core R215-ARCH-P1-2 guard: future code added between
// ++ and -- that panics MUST NOT strand the counter.
func TestPendingSpawnSlot_DeferReleaseAbsorbsPanic(t *testing.T) {
	t.Parallel()

	r := &Router{ss: newSessionTable()}

	func() {
		defer func() {
			// We expect a panic — recover and continue. The slot's defer
			// fires BEFORE this recover (LIFO defer order), so by the
			// time we land here pendingSpawns is already back to 0.
			_ = recover()
		}()

		var slot pendingSpawnSlot
		r.ss.Update(func(tx sessTx) { slot = r.acquirePendingSpawnSlot(tx) })
		defer slot.release()

		// Simulate a panic between ++ and the matching -- (e.g., the
		// "future refactor introduces panic in the other 3 segments"
		// scenario from the issue).
		panic("synthetic panic between ++ and --")
	}()

	var got int
	r.ss.Update(func(tx sessTx) {
		got = tx.Ext().spawns.PendingSpawns()
	})
	if got != 0 {
		t.Fatalf("pendingSpawns=%d after panic-then-defer-release, want 0 (counter would otherwise strand permanently and every GetOrCreate would refuse with ErrMaxProcs until restart)", got)
	}
}

// TestPendingSpawnSlot_ReleaseTakesLock: when releaseIn was never
// called, release() must acquire the table lock itself and decrement.
func TestPendingSpawnSlot_ReleaseTakesLock(t *testing.T) {
	t.Parallel()

	r := &Router{ss: newSessionTable()}
	var slot pendingSpawnSlot
	r.ss.Update(func(tx sessTx) {
		slot = r.acquirePendingSpawnSlot(tx)
	})

	slot.release() // must self-lock + decrement

	var got int
	r.ss.Update(func(tx sessTx) {
		got = tx.Ext().spawns.PendingSpawns()
	})
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
	slot.releaseIn(sessTx{})
}

// TestPendingSpawnSlot_DoubleReleasePathsAreSafe: the spawn's happy
// path calls releaseIn() inline and the function-level defer also
// fires release(). Both must net to a single decrement (single
// goroutine — the spawn is the only owner of the slot, so this is
// not a concurrent test, just a sequential idempotency pin matching
// the actual production call shape).
func TestPendingSpawnSlot_DoubleReleasePathsAreSafe(t *testing.T) {
	t.Parallel()

	r := &Router{ss: newSessionTable()}
	var slot pendingSpawnSlot
	r.ss.Update(func(tx sessTx) {
		slot = r.acquirePendingSpawnSlot(tx)
	})

	// First: simulate the post-Spawn re-lock + releaseIn happy path.
	r.ss.Update(func(tx sessTx) {
		slot.releaseIn(tx)
	})

	// Then: the deferred release() at function exit must be a no-op.
	slot.release()

	var got int
	r.ss.Update(func(tx sessTx) {
		got = tx.Ext().spawns.PendingSpawns()
	})
	if got != 0 {
		t.Fatalf("pendingSpawns=%d after happy-path releaseIn + defer release, want 0", got)
	}
}
