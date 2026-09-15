package shim

// Teardown paths: stopping, detaching and forgetting shims, plus the zombie
// cleanup that runs when a discovered shim turns out to be unusable. Split out
// of manager.go (#2713 B6).

import (
	"context"
	"log/slog"
	"sync"
)

// ForceCleanupZombie purges a shim whose reconnect is irrecoverable: removes
// its state file and best-effort SIGTERMs the process. Used by the router on
// repeated socket ENOENT instead of waiting up to 30s for the next Discover
// tick. PID 0 or empty key are no-ops.
//
// The PID's binary identity is re-validated before signalling (PID reuse
// between Reconnect's check and this call); a mismatch skips the kill but
// still removes the state file.
func (m *Manager) ForceCleanupZombie(state State) {
	// Remove the state file BEFORE SIGTERM so a concurrent reconnectShims tick
	// cannot see the file + a still-alive PID and attach to a dying shim.
	// Discover reads the filesystem, not the map.
	keyHash := KeyHash(state.Key)
	RemoveStateFile(StateFilePath(m.stateDir, keyHash))
	m.mu.Lock()
	delete(m.shims, state.Key)
	m.mu.Unlock()
	if state.ShimPID > 0 && m.isOurShimPID(state.ShimPID) {
		_ = sendSIGTERM(state.ShimPID)
	}
}

// isOurShimPID reports whether pid is alive AND its binary matches the naozhi
// binary we launched from (same gate as Discover). Run it before signalling
// any PID learned from a state file.
func (m *Manager) isOurShimPID(pid int) bool {
	if !pidAlive(pid) {
		return false
	}
	mismatch, err := shimPIDBinaryMismatch(pid, m.naozhiBin)
	if err != nil {
		// Cannot confirm identity: do not signal unknown PIDs.
		return false
	}
	return !mismatch
}

// StopAll sends shutdown to all known shims concurrently. ctx bounds how long
// the caller blocks for the drain; on expiry StopAll returns early and logs
// the in-flight count while the goroutines finish on their own (abandon the
// tail rather than block the systemd shutdown watchdog).
func (m *Manager) StopAll(ctx context.Context) {
	m.mu.Lock()
	handles := make(map[string]*ShimHandle, len(m.shims))
	for k, v := range m.shims {
		handles[k] = v
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for key, h := range handles {
		wg.Add(1)
		go func(k string, h *ShimHandle) {
			defer wg.Done()
			slog.Info("shutting down shim", "key", k)
			h.Shutdown()
		}(key, h)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		// Also drain the reaper goroutines so callers see a fully-drained
		// Manager; they are bounded by shim lifetime, and the outer ctx still
		// bounds wall-clock if one is stuck (#565).
		m.reaperWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn("shim.Manager.StopAll: ctx expired before drain",
			"err", ctx.Err(),
			"pending_shims", len(handles))
	}
}

// DetachAll sends detach to all known shims concurrently (used during graceful shutdown).
func (m *Manager) DetachAll() {
	m.mu.Lock()
	handles := make(map[string]*ShimHandle, len(m.shims))
	for k, v := range m.shims {
		handles[k] = v
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, h := range handles {
		wg.Add(1)
		go func(h *ShimHandle) {
			defer wg.Done()
			h.Detach()
		}(h)
	}
	wg.Wait()
}

// Remove removes a shim handle from the manager's tracking.
func (m *Manager) Remove(key string) {
	m.mu.Lock()
	delete(m.shims, key)
	m.mu.Unlock()

	// Also drop the per-key reconnect mutex so reconnectKM does not grow with
	// the lifetime count of keys (#2251). Take reconnectMu separately, NEVER
	// while holding m.mu (lock order). A racing Reconnect simply re-creates
	// the entry, which is correct.
	m.reconnectMu.Lock()
	delete(m.reconnectKM, key)
	m.reconnectMu.Unlock()
}

// removeShimIfCurrent deletes key from m.shims only when the stored handle is
// still `want`. It is the reaper's map-cleanup hook: when a spawned shim exits,
// its entry is dropped so the admission count (len(m.shims)+pendingShims vs
// maxShims) reflects live shims rather than lifetime distinct keys.
//
// The identity check is load-bearing: a concurrent StartShim/Reconnect for the
// same key may have swapped in a fresh live handle, and an unconditional delete
// from the old process's reaper would strand it (uncounted, unfindable). A
// superseded reaper is a no-op. reconnectKM is deliberately untouched — a live
// replacement still needs its per-key mutex; only Manager.Remove reclaims it.
func (m *Manager) removeShimIfCurrent(key string, want *ShimHandle) {
	m.mu.Lock()
	if m.shims[key] == want {
		delete(m.shims, key)
	}
	m.mu.Unlock()
}
