package project

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestResolveWorkspaces_ConcurrentWithWriters is the -race regression for
// #2228: concurrent ResolveWorkspaces (reads taking the snapshot) and writers
// (Scan / UpdateConfig replacing or mutating m.projects) must be race-free. The
// fix introduced a projRef snapshot built under the RLock; this exercises that
// snapshot against concurrent writes.
func TestResolveWorkspaces_ConcurrentWithWriters(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: skipped in -short")
	}
	t.Parallel()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "real")
	makeProjectDir(t, root, "proj", nil)
	makeProjectDir(t, root, "other", nil)

	m, err := NewManager(root, PlannerDefaults{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Scan(); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	alias := filepath.Join(tmp, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	stop := make(chan struct{})
	var readers, writers sync.WaitGroup

	// Many readers hitting both fast path and fallback.
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				m.ResolveWorkspaces([]string{
					filepath.Join(root, "proj", "sub"),     // fast path
					filepath.Join(alias, "proj", "nested"), // fallback
				})
			}
		}()
	}

	// Writers: Scan replaces m.projects (and clears the cache) while UpdateConfig
	// mutates Config under the write lock.
	//
	// 60 rather than the original 200: each Scan is a filesystem walk, so the loop
	// count IS this test's wall time (200 → 7.98s under -race). Detection does not
	// come from the count — TSan reports a race the first time it observes two
	// conflicting accesses with no happens-before edge, and the readers above spin
	// FREELY until the writers finish, so their coverage is bounded by wall time.
	//
	// VERIFIED rather than assumed: moving the projRef snapshot back outside the
	// RLock (the #2228 bug) makes this test report DATA RACE at 60, three runs out
	// of three.
	for w := 0; w < 2; w++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := 0; i < 60; i++ {
				if i%2 == 0 {
					_ = m.Scan()
				} else {
					_ = m.UpdateConfig("proj", ProjectConfig{Favorite: i%4 == 1})
				}
			}
		}()
	}

	writers.Wait()
	close(stop)
	readers.Wait()
}

// TestResolveWorkspaceByInode_StaleSnapshotNotCached is the deterministic
// regression for the #2228 follow-up: because the inode fallback now Stores into
// resolveCache lock-free (after RUnlock), a Scan landing in that window must not
// let a name computed from the OLD project snapshot survive the rescan. The
// generation guard detects the intervening Scan (resolveGen moved) and rolls the
// stale Store back so the next tick re-resolves against the fresh project set.
func TestResolveWorkspaceByInode_StaleSnapshotNotCached(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "real")
	makeProjectDir(t, root, "proj", nil)

	m, err := NewManager(root, PlannerDefaults{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Scan(); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	ws := filepath.Join(root, "proj", "sub")
	snap := []projRef{{name: "proj", path: filepath.Join(root, "proj")}}

	// Simulate a Scan that lands between the caller's snapshot (snapGen) and the
	// Store: pass a stale snapGen that no longer matches the live resolveGen.
	staleGen := m.resolveGen.Load() - 1
	if got := m.resolveWorkspaceByInode(ws, snap, staleGen); got != "proj" {
		t.Fatalf("resolveWorkspaceByInode = %q, want %q", got, "proj")
	}
	if _, ok := m.resolveCache.Load(ws); ok {
		t.Fatalf("stale snapshot result was cached across a rescan; generation guard failed (#2228)")
	}

	// A current-generation snapshot caches normally.
	curGen := m.resolveGen.Load()
	if got := m.resolveWorkspaceByInode(ws, snap, curGen); got != "proj" {
		t.Fatalf("resolveWorkspaceByInode (fresh gen) = %q, want %q", got, "proj")
	}
	if _, ok := m.resolveCache.Load(ws); !ok {
		t.Fatalf("current-generation result should be cached")
	}
}
