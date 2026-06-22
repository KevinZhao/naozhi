package project

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestResolveWorkspaces_FallbackDoesNotHoldLockDuringFSIO is the regression
// test for #2228: ResolveWorkspaces used to hold m.mu.RLock across the
// inode-fallback FS IO (resolveWorkspaceByInode → os.Lstat per ancestor),
// blocking every writer (BindChat / UpdateConfig / Scan, all m.mu.Lock) for
// the syscall fan-out. After the fix the snapshot is taken under the RLock and
// the FS IO runs lock-free, so a writer can acquire m.mu.Lock while resolution
// is in flight.
//
// We can't slow os.Lstat directly, so we prove the property structurally: while
// one goroutine spins ResolveWorkspaces on a path that ALWAYS hits the fallback
// (byte prefix misses, cache disabled by using a fresh ws each iteration), a
// writer repeatedly grabs the write lock. If the fallback held the RLock across
// the walk the writer would observe long stalls; we assert every write-lock
// acquisition completes promptly.
func TestResolveWorkspaces_FallbackDoesNotHoldLockDuringFSIO(t *testing.T) {
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

	// alias names the same inode as root, so a ws under alias byte-misses the
	// project Path but inode-walks back to it: the fallback always runs.
	alias := filepath.Join(tmp, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	stop := make(chan struct{})
	var readers sync.WaitGroup

	// Resolver: each iteration uses a UNIQUE ws so resolveCache never short-
	// circuits and the os.Lstat walk genuinely executes every loop.
	readers.Add(1)
	go func() {
		defer readers.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			ws := filepath.Join(alias, "proj", "sub", strings.Repeat("a/", i%8+1))
			m.ResolveWorkspaces([]string{ws})
			i++
		}
	}()

	// Writer: every UpdateConfig takes m.mu.Lock. Measure how long each
	// acquisition+work takes; if the resolver held the RLock across FS IO these
	// would block far longer than a lock-free in-memory config swap should.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		start := time.Now()
		if err := m.UpdateConfig("proj", ProjectConfig{Favorite: true}); err != nil {
			t.Fatalf("UpdateConfig: %v", err)
		}
		if waited := time.Since(start); waited > 2*time.Second {
			t.Fatalf("UpdateConfig blocked %v waiting for write lock; "+
				"ResolveWorkspaces fallback appears to hold the RLock across FS IO (#2228)", waited)
		}
	}

	close(stop)
	readers.Wait()
}

// TestResolveWorkspaces_ConcurrentWithWriters is the -race regression for
// #2228: concurrent ResolveWorkspaces (reads taking the snapshot) and writers
// (Scan / UpdateConfig replacing or mutating m.projects) must be race-free. The
// fix introduced a projRef snapshot built under the RLock; this exercises that
// snapshot against concurrent writes.
func TestResolveWorkspaces_ConcurrentWithWriters(t *testing.T) {
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
	for w := 0; w < 2; w++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := 0; i < 200; i++ {
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
