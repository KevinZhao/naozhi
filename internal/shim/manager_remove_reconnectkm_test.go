package shim

import (
	"sync"
	"testing"
)

// TestRemove_ReclaimsReconnectKM pins #2251: Remove(key) must drop the
// per-key reconnect mutex from reconnectKM, not just the shim handle from
// shims. reconnectKey lazily inserts into reconnectKM and nothing else ever
// deletes, so a long-lived Manager that churns through distinct session keys
// would grow reconnectKM without bound (a slow memory leak).
func TestRemove_ReclaimsReconnectKM(t *testing.T) {
	t.Parallel()
	m := &Manager{
		shims:       make(map[string]*ShimHandle),
		reconnectKM: make(map[string]*sync.Mutex),
	}

	keys := []string{"a", "b", "c"}
	for _, k := range keys {
		// Register a handle slot and force the lazy reconnectKM insert.
		m.mu.Lock()
		m.shims[k] = &ShimHandle{}
		m.mu.Unlock()
		_ = m.reconnectKey(k)
	}

	m.reconnectMu.Lock()
	if got := len(m.reconnectKM); got != len(keys) {
		m.reconnectMu.Unlock()
		t.Fatalf("setup: reconnectKM len = %d, want %d", got, len(keys))
	}
	m.reconnectMu.Unlock()

	for _, k := range keys {
		m.Remove(k)
	}

	m.mu.Lock()
	shimsLeft := len(m.shims)
	m.mu.Unlock()
	if shimsLeft != 0 {
		t.Errorf("after Remove, shims len = %d, want 0", shimsLeft)
	}

	m.reconnectMu.Lock()
	kmLeft := len(m.reconnectKM)
	m.reconnectMu.Unlock()
	if kmLeft != 0 {
		t.Errorf("after Remove, reconnectKM len = %d, want 0 — per-key reconnect "+
			"mutexes leaked (#2251); the map grows monotonically with distinct "+
			"session keys over the Manager lifetime", kmLeft)
	}
}

// TestRemove_ReconnectKMReinsertAfterRace pins the documented race semantics:
// a Reconnect (via reconnectKey) racing a Remove on the same key just
// re-creates the entry, which is correct (the session is gone, a fresh mutex
// is equivalent). We model it deterministically: Remove, then reconnectKey
// must succeed and re-populate the slot.
func TestRemove_ReconnectKMReinsertAfterRace(t *testing.T) {
	t.Parallel()
	m := &Manager{
		shims:       make(map[string]*ShimHandle),
		reconnectKM: make(map[string]*sync.Mutex),
	}
	const key = "k"
	first := m.reconnectKey(key)
	m.Remove(key)

	m.reconnectMu.Lock()
	_, present := m.reconnectKM[key]
	m.reconnectMu.Unlock()
	if present {
		t.Fatal("Remove did not delete reconnectKM[key]")
	}

	second := m.reconnectKey(key)
	if second == nil {
		t.Fatal("reconnectKey after Remove returned nil")
	}
	if second == first {
		t.Error("reconnectKey after Remove returned the stale pre-Remove mutex; " +
			"expected a freshly-created one")
	}
}
