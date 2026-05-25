package cron

// R247-PERF-24 regression tests. workDirResolveCache must:
//   - serve a positive resolution from the in-memory map without the
//     pure helper running EvalSymlinks on the second call;
//   - expire entries after workDirResolveCacheTTL so a symlink retarget
//     becomes visible on the next tick;
//   - bypass the cache for negative results so a workspace that briefly
//     vanishes does not leak a stale "ok=false" forever once it returns.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkDirResolveCache_HitMiss(t *testing.T) {
	t.Parallel()
	c := &workDirResolveCache{}
	now := time.Now()
	if _, ok := c.lookup("k", now); ok {
		t.Fatalf("empty cache lookup returned ok=true")
	}
	c.store("k", "/resolved", now)
	got, ok := c.lookup("k", now)
	if !ok || got != "/resolved" {
		t.Fatalf("after store: got=%q ok=%v want=/resolved ok=true", got, ok)
	}
}

func TestWorkDirResolveCache_TTLExpiry(t *testing.T) {
	t.Parallel()
	c := &workDirResolveCache{}
	now := time.Now()
	c.store("k", "/r", now)
	// Just inside TTL → still hit.
	if _, ok := c.lookup("k", now.Add(workDirResolveCacheTTL-time.Millisecond)); !ok {
		t.Fatalf("inside TTL: lookup returned ok=false")
	}
	// At TTL boundary → expired (lookup uses !before(expiresAt) so equal=expired).
	if _, ok := c.lookup("k", now.Add(workDirResolveCacheTTL)); ok {
		t.Fatalf("at TTL boundary: lookup returned ok=true (want expired)")
	}
}

func TestWorkDirResolveCache_NilSafe(t *testing.T) {
	t.Parallel()
	var c *workDirResolveCache
	if _, ok := c.lookup("k", time.Now()); ok {
		t.Fatalf("nil receiver lookup returned ok=true")
	}
	// Should not panic.
	c.store("k", "/r", time.Now())
}

func TestWorkDirResolveCache_KeyDistinguishesInputs(t *testing.T) {
	t.Parallel()
	a := workDirResolveCacheKey("/work", "/root", "/rootResolved")
	b := workDirResolveCacheKey("/wor", "k/root", "/rootResolved")
	// Naive concat would collide ("/work"+"/root" == "/wor"+"k/root") —
	// the \x00 separator is what prevents that.
	if a == b {
		t.Fatalf("key collision: a=%q b=%q (separator missing?)", a, b)
	}
}

// TestWorkDirResolveUnderRootCached_HitElidesEvalSymlinks proves that a
// second call with the same inputs does not re-run EvalSymlinks. We can't
// stub the syscall directly, so we observe behaviour via a workspace
// that we delete after the first call: without the cache the second
// call would now return ok=false; with the cache the prior positive
// answer is reused while the TTL is alive.
func TestWorkDirResolveUnderRootCached_HitElidesEvalSymlinks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workDir := filepath.Join(root, "child")
	if err := os.Mkdir(workDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(root): %v", err)
	}
	s := &Scheduler{
		allowedRoot:         root,
		allowedRootResolved: rootResolved,
	}
	resolved, ok := s.workDirResolveUnderRootCached(workDir)
	if !ok {
		t.Fatalf("first call: ok=false, want ok=true")
	}
	expected, _ := filepath.EvalSymlinks(workDir)
	if resolved != expected {
		t.Fatalf("first call: resolved=%q want=%q", resolved, expected)
	}
	// Remove the workspace. A bypass-cache resolve would now return ok=false.
	if err := os.RemoveAll(workDir); err != nil {
		t.Fatalf("rmdir: %v", err)
	}
	resolved2, ok2 := s.workDirResolveUnderRootCached(workDir)
	if !ok2 || resolved2 != expected {
		t.Fatalf("second call (workspace removed but TTL alive): resolved=%q ok=%v want=%q ok=true",
			resolved2, ok2, expected)
	}
}

// TestWorkDirResolveUnderRootCached_NegativeBypassesCache verifies that
// a negative answer is NOT cached: a workspace that briefly disappears
// and is restored should be reachable on the very next call. Without
// this, a transient EvalSymlinks failure would refuse the job for a
// full TTL window.
func TestWorkDirResolveUnderRootCached_NegativeBypassesCache(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	rootResolved, _ := filepath.EvalSymlinks(root)
	missing := filepath.Join(root, "not-yet-created")
	s := &Scheduler{
		allowedRoot:         root,
		allowedRootResolved: rootResolved,
	}
	if _, ok := s.workDirResolveUnderRootCached(missing); ok {
		t.Fatalf("missing workspace: ok=true, want ok=false")
	}
	// Create the workspace; the next call must succeed (no negative cache).
	if err := os.Mkdir(missing, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, ok := s.workDirResolveUnderRootCached(missing); !ok {
		t.Fatalf("after restore: ok=false, want ok=true (negative result must not be cached)")
	}
}

// BenchmarkWorkDirResolveUnderRootCached approximates the syscall savings
// the cache delivers. Cached path is a sync.Map.Load + time.Now compare;
// uncached path is an EvalSymlinks chain (Lstat+Readlink per component).
func BenchmarkWorkDirResolveUnderRootCached(b *testing.B) {
	root := b.TempDir()
	workDir := filepath.Join(root, "child")
	if err := os.Mkdir(workDir, 0o700); err != nil {
		b.Fatalf("mkdir: %v", err)
	}
	rootResolved, _ := filepath.EvalSymlinks(root)
	s := &Scheduler{
		allowedRoot:         root,
		allowedRootResolved: rootResolved,
	}
	// Warm.
	if _, ok := s.workDirResolveUnderRootCached(workDir); !ok {
		b.Fatalf("warm: ok=false")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.workDirResolveUnderRootCached(workDir)
	}
}
