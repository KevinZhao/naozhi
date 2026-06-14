package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestSpawnSession_RechecksPendingAfterEvictWindow pins #2082.
//
// spawnSession's non-exempt capacity admission snapshots pendingSpawns under
// r.mu (router_lifecycle.go:707). When at capacity it calls evictOldest(),
// which releases and re-acquires r.mu around proc.Close() (router_capacity.go).
// A concurrent spawnSession can ++ pendingSpawns inside that unlock window. The
// post-evict recheck must therefore re-read pendingSpawns rather than reuse the
// pre-evict snapshot — otherwise a stale (smaller) value lets us over-spawn past
// maxProcs.
//
// Setup: maxProcs=1 with a single idle (evictable) session. The evictable
// process's Close() hook runs inside evictOldest's unlock window and bumps
// pendingSpawns to 1, simulating a concurrent spawn that grabbed a slot. After
// the evict, activeCount drops to 0, so the buggy code (reusing the stale
// pending64=0) would see 0+0 < 1 and proceed to spawn (failing only on the
// nonexistent CLI with a "spawn" error, i.e. over-spawn admitted). The fixed
// code re-reads pendingSpawns=1, sees 0+1 >= 1, and correctly refuses with
// ErrMaxProcs.
func TestSpawnSession_RechecksPendingAfterEvictWindow(t *testing.T) {
	r := newTestRouter(1)

	hook := newHookCloseProc(func() {
		// Runs after evictOldest has released r.mu for proc.Close(). Mimic a
		// concurrent spawnSession that acquired a pending slot in the window.
		r.mu.Lock()
		r.pp.pendingSpawns++
		r.mu.Unlock()
	})
	old := injectSession(r, "old-key", hook)
	old.lastActive.Store(time.Now().Add(-1 * time.Hour).UnixNano())

	_, _, err := r.GetOrCreate(context.Background(), "new-key", AgentOpts{})
	if err == nil {
		t.Fatal("expected ErrMaxProcs after evict window raised pendingSpawns to capacity, got nil")
	}
	if !errors.Is(err, ErrMaxProcs) {
		t.Fatalf("expected ErrMaxProcs (recheck must re-read pendingSpawns post-evict), got: %v", err)
	}
	if strings.Contains(err.Error(), "spawn process") {
		t.Fatalf("over-spawn admitted: capacity check reused stale pending64 and proceeded to Spawn, got: %v", err)
	}
	// The evictable process must still have been Close()'d (eviction happened).
	if old.loadProcess().Alive() {
		t.Error("evictable process should have been closed by evictOldest")
	}
}
