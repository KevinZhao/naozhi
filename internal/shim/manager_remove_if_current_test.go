package shim

import (
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// TestRemoveShimIfCurrent_DeletesMatchingHandle is the core regression for the
// "max shims reached (50)" leak: a spawned shim's reaper must drop the dead
// entry from m.shims so the admission count (len(m.shims)+pendingShims) stops
// growing unbounded across distinct session keys. Before the fix, m.shims only
// shrank via ForceCleanupZombie / the never-called Manager.Remove, so normal
// shim death left the entry behind.
//
// This pins the happy path: removeShimIfCurrent(key, h) deletes key when the
// stored handle IS h.
func TestRemoveShimIfCurrent_DeletesMatchingHandle(t *testing.T) {
	m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})

	key := "dashboard:direct:2026-06-28-225345-2-x:general"
	h := &ShimHandle{}
	m.mu.Lock()
	m.shims[key] = h
	m.mu.Unlock()

	m.removeShimIfCurrent(key, h)

	m.mu.Lock()
	_, exists := m.shims[key]
	n := len(m.shims)
	m.mu.Unlock()
	if exists {
		t.Error("removeShimIfCurrent did not delete the matching handle from m.shims")
	}
	if n != 0 {
		t.Errorf("len(m.shims) = %d, want 0", n)
	}
}

// TestRemoveShimIfCurrent_PreservesReplacedHandle is the load-bearing
// concurrency invariant. When a key is re-spawned (the oldHandle.Close() swap
// in StartShimWithBackend / Reconnect), the OLD process's reaper must NOT evict
// the NEW live handle. removeShimIfCurrent compares pointers and no-ops when
// the stored handle has been replaced — otherwise a running shim would be
// stranded (alive on the OS, untracked, no longer counting toward maxShims,
// and unreachable to the next Reconnect's map lookup).
func TestRemoveShimIfCurrent_PreservesReplacedHandle(t *testing.T) {
	m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})

	key := "feishu:direct:alice:general"
	oldHandle := &ShimHandle{}
	newHandle := &ShimHandle{}

	// Simulate: old shim spawned, then a re-spawn swapped in newHandle under
	// the same key (as StartShimWithBackend's map-insert section does).
	m.mu.Lock()
	m.shims[key] = newHandle
	m.mu.Unlock()

	// The OLD process's reaper now fires with its stale handle snapshot.
	m.removeShimIfCurrent(key, oldHandle)

	m.mu.Lock()
	got := m.shims[key]
	m.mu.Unlock()
	if got != newHandle {
		t.Fatalf("removeShimIfCurrent evicted the live replacement handle: got %p, want %p (the old reaper must be a no-op once the key was re-spawned)", got, newHandle)
	}
}

// TestRemoveShimIfCurrent_AbsentKeyNoPanic guards the spawn-failed path: the
// reaper's nil-snapshot guard means removeShimIfCurrent is only reached with a
// real handle, but a key already swept by a terminal Remove must still be a
// safe no-op rather than a panic.
func TestRemoveShimIfCurrent_AbsentKeyNoPanic(t *testing.T) {
	m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
	m.removeShimIfCurrent("never:inserted:key", &ShimHandle{}) // must not panic
}

// TestRemoveShimIfCurrent_ConcurrentReapersOneKey stresses the identity check
// under concurrency: N reapers for superseded handles plus one live handle.
// Only the live handle must survive, and exactly the superseded entries are
// removed. Run under -race to catch any m.mu mishandling.
func TestRemoveShimIfCurrent_ConcurrentReapersOneKey(t *testing.T) {
	m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
	key := "k:p:1"

	live := &ShimHandle{}
	m.mu.Lock()
	m.shims[key] = live
	m.mu.Unlock()

	const stale = 16
	var wg sync.WaitGroup
	for i := 0; i < stale; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each stale reaper holds a different (superseded) handle.
			m.removeShimIfCurrent(key, &ShimHandle{})
		}()
	}
	wg.Wait()

	m.mu.Lock()
	got := m.shims[key]
	m.mu.Unlock()
	if got != live {
		t.Fatalf("live handle was evicted by a stale reaper: got %p, want %p", got, live)
	}
}

// TestReaperHandleStore_HappensUnderLock pins the Store-ordering invariant that
// a code reviewer caught: reaperHandle.Store(handle) MUST happen inside the
// m.mu critical section that installs the map entry, BEFORE m.mu.Unlock().
//
// If the Store is moved after the Unlock, a fast-dying shim's reaper can run
// reaperHandle.Load() in the window between Unlock and Store, observe nil, skip
// removeShimIfCurrent, and leak the map entry forever — re-introducing the
// "max shims reached" bug for the spawn-success-then-immediate-death path. A
// source-level scan is the right altitude here (a runtime test cannot reliably
// hit the sub-microsecond window); it locks in the ordering so a future refactor
// that hoists the Store out of the lock trips this test instead of production.
func TestReaperHandleStore_HappensUnderLock(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("manager.go")
	if err != nil {
		t.Skipf("manager.go not readable in cwd; skipping source-shape pin: %v", err)
	}
	body := string(src)

	// Scope the scan to StartShimWithBackend.
	startIdx := strings.Index(body, "func (m *Manager) StartShimWithBackend(")
	if startIdx < 0 {
		t.Fatal("StartShimWithBackend not found in manager.go — update this contract test if it was renamed.")
	}
	rest := body[startIdx:]
	if endRel := regexp.MustCompile(`\nfunc `).FindStringIndex(rest[6:]); endRel != nil {
		rest = rest[:6+endRel[0]]
	}

	storeIdx := strings.Index(rest, "reaperHandle.Store(handle)")
	if storeIdx < 0 {
		t.Fatal("reaperHandle.Store(handle) not found in StartShimWithBackend — the reaper map-cleanup publish step was removed, which re-leaks m.shims.")
	}
	insertIdx := strings.Index(rest, "m.shims[key] = handle")
	if insertIdx < 0 {
		t.Fatal("m.shims[key] = handle not found in StartShimWithBackend.")
	}
	// The Store must come AFTER the map insert (so the reaper never sees a
	// handle that isn't yet in the map) ...
	if storeIdx < insertIdx {
		t.Fatal("reaperHandle.Store(handle) appears BEFORE m.shims[key] = handle — publish the handle only after it is installed in the map.")
	}
	// ... and BEFORE the Unlock that follows the insert. Find the first
	// m.mu.Unlock() at or after the map insert; the Store must precede it.
	unlockRel := strings.Index(rest[insertIdx:], "m.mu.Unlock()")
	if unlockRel < 0 {
		t.Fatal("no m.mu.Unlock() found after m.shims[key] = handle in StartShimWithBackend.")
	}
	unlockIdx := insertIdx + unlockRel
	if storeIdx > unlockIdx {
		t.Fatalf("reaperHandle.Store(handle) (offset %d) appears AFTER the m.mu.Unlock() (offset %d) that closes the map-insert critical section. The Store MUST be inside the lock — otherwise a fast-dying shim's reaper Loads nil in the Unlock→Store window, skips the map delete, and leaks the entry (the 'max shims reached' bug). Move the Store before m.mu.Unlock().", storeIdx, unlockIdx)
	}
}
