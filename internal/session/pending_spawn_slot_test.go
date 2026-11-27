package session

// R215-ARCH-P1-2 regression tests for the pending-spawn RAII slot token:
// releaseIn is idempotent so the happy-path decrement plus a deferred
// releaseIn net to exactly one decrement.

import (
	"testing"
)

// TestPendingSpawnSlot_ReleaseInIsIdempotent: an explicit releaseIn runs
// once; a second releaseIn (e.g. from defer) must be a no-op.
func TestPendingSpawnSlot_ReleaseInIsIdempotent(t *testing.T) {
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

	r.ss.Update(func(tx sessTx) {
		slot.releaseIn(tx)
		if tx.Ext().spawns.PendingSpawns() != 0 {
			t.Fatalf("pendingSpawns=%d after redundant releaseIn, want 0 (idempotent)", tx.Ext().spawns.PendingSpawns())
		}
	})
}
