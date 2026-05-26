package shim

import (
	"sync"
	"testing"
)

// TestReconnect_PerKeyMutexExists pins R51-CONCUR-005: Manager owns a
// per-key mutex pool used by Reconnect to serialise concurrent attempts
// on the same key. Without it, two callers could each finish their own
// dial, then the late winner's swap would close the early winner's
// handle while Router has it attached to a live Process.
//
// The contract this test pins is structural — the runtime race needs a
// real shim to reproduce, but the per-key pool primitive is what makes
// the fix possible. If a future refactor removes the pool, the runtime
// race re-emerges and Router-attached handles get spuriously Closed on
// reconcile fan-out.
func TestReconnect_PerKeyMutexExists(t *testing.T) {
	t.Parallel()
	m := &Manager{
		shims:       make(map[string]*ShimHandle),
		reconnectKM: make(map[string]*sync.Mutex),
	}
	mu1 := m.reconnectKey("k1")
	mu2 := m.reconnectKey("k2")
	if mu1 == mu2 {
		t.Error("reconnectKey returned the same mutex for two different keys; " +
			"per-key serialisation is broken (one global mutex across all keys " +
			"defeats the cross-key parallelism R49-REL-SHIM-MANAGER-RECONNECT-CONCUR " +
			"specifically calls out as a non-goal)")
	}
	mu1b := m.reconnectKey("k1")
	if mu1 != mu1b {
		t.Error("reconnectKey returned different mutexes for the same key " +
			"on consecutive calls; serialisation is per-key only when both " +
			"callers see the SAME mutex")
	}
}

// TestReconnect_PerKeyMutexConcurrentSafe locks the contract that
// reconnectKey itself is goroutine-safe. The map lookup is guarded by
// reconnectMu; without it two concurrent reconnectKey calls for the
// same key could create two distinct mutexes, defeating the
// serialisation invariant.
func TestReconnect_PerKeyMutexConcurrentSafe(t *testing.T) {
	t.Parallel()
	m := &Manager{
		shims:       make(map[string]*ShimHandle),
		reconnectKM: make(map[string]*sync.Mutex),
	}

	const goroutines = 32
	results := make([]*sync.Mutex, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			results[i] = m.reconnectKey("shared-key")
		}(i)
	}
	wg.Wait()

	first := results[0]
	for i, mu := range results {
		if mu != first {
			t.Errorf("goroutine %d got a different mutex than goroutine 0; "+
				"reconnectKey is not concurrent-safe", i)
		}
	}
}
