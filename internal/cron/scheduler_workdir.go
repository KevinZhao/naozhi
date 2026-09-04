package cron

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// workDirReachable reports whether workDir exists and is a directory right
// now, so fresh-mode Reset does not destroy a session only to fail on spawn.
// Empty workDir means "use router default" and is always reachable.
//
// 注意：仅做 stat 可达性 + IsDir 检查，**不**强制 allowedRoot 内含；依赖
// "必须在工作根之内"的调用者必须额外调 workDirUnderRoot。
func workDirReachable(workDir string) bool {
	if workDir == "" {
		return true
	}
	info, err := os.Stat(workDir)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// workDirUnderRoot reports whether workDir resolves (after symlink evaluation)
// to a path at or under allowedRoot. EvalSymlinks runs per call for both sides
// so the check reflects current filesystem state, closing the TOCTOU between
// creation-time validateWorkspace and execute-time binding (and a retargeted
// allowedRoot symlink). Both arguments must be absolute. allowedRootResolved,
// when non-empty, is a fallback used only if EvalSymlinks(allowedRoot) fails.
func workDirUnderRoot(workDir, allowedRoot, allowedRootResolved string) bool {
	_, ok := workDirResolveUnderRoot(workDir, allowedRoot, allowedRootResolved)
	return ok
}

// workDirResolveCacheTTL caps how long a positive workDirResolveUnderRoot
// result is reused before re-running EvalSymlinks (Lstat+Readlink per path
// component, every tick). 30s bounds the TOCTOU window the per-call
// EvalSymlinks exists to close; only positive results are cached so a
// restored workspace is re-resolved on the next call (#572).
const workDirResolveCacheTTL = 30 * time.Second

// workDirResolveCacheEntry captures one cached resolution. Stored value-
// typed in sync.Map so the read path does no allocation.
type workDirResolveCacheEntry struct {
	resolved  string
	expiresAt time.Time
}

// workDirResolveCacheMaxEntries caps the cached (workDir, allowedRoot,
// allowedRootResolved) tuples. workDir is operator-controlled and entries
// expire only on read of the same key, so without a cap a caller varying
// WorkDir per call (trailing slash, NFC/NFD) grows the map indefinitely (#1273).
// Over the cap, store sweeps expired entries first and otherwise drops the
// write — the cache is an optimisation, not a correctness path. 4096 ≈ 1.5 MB.
const workDirResolveCacheMaxEntries = 4096

// workDirResolveCache memoises positive workDirResolveUnderRoot results keyed
// by raw (workDir,allowedRoot,allowedRootResolved). Negative results bypass
// the cache; entries expire lazily on read.
type workDirResolveCache struct {
	m     sync.Map     // map[string]workDirResolveCacheEntry
	count atomic.Int64 // approximate live entries; allowed to drift slightly
}

// lookup returns the cached resolution for key if present and unexpired.
func (c *workDirResolveCache) lookup(key string, now time.Time) (string, bool) {
	if c == nil {
		return "", false
	}
	v, ok := c.m.Load(key)
	if !ok {
		return "", false
	}
	e := v.(workDirResolveCacheEntry)
	if !now.Before(e.expiresAt) {
		// Expired: drop it. LoadAndDelete keeps c.count in sync (concurrent expirers
		// won't double-decrement).
		if _, deleted := c.m.LoadAndDelete(key); deleted {
			c.count.Add(-1)
		}
		return "", false
	}
	return e.resolved, true
}

// sweepExpired walks the map once dropping any entry whose expiresAt has
// passed. Called only on the over-cap branch of store; sync.Map.Range is
// O(N) but bounded by workDirResolveCacheMaxEntries. Concurrent lookups
// remain race-free — Range observes a consistent snapshot per Go's docs.
func (c *workDirResolveCache) sweepExpired(now time.Time) {
	c.m.Range(func(k, v any) bool {
		e, ok := v.(workDirResolveCacheEntry)
		if !ok || !now.Before(e.expiresAt) {
			if _, deleted := c.m.LoadAndDelete(k); deleted {
				c.count.Add(-1)
			}
		}
		return true
	})
}

func (c *workDirResolveCache) store(key, resolved string, now time.Time) {
	if c == nil {
		return
	}
	// At/over the cap: sweep expired entries first; if every entry is still warm,
	// drop the write.
	if c.count.Load() >= workDirResolveCacheMaxEntries {
		c.sweepExpired(now)
		if c.count.Load() >= workDirResolveCacheMaxEntries {
			return
		}
	}
	if _, loaded := c.m.LoadOrStore(key, workDirResolveCacheEntry{
		resolved:  resolved,
		expiresAt: now.Add(workDirResolveCacheTTL),
	}); !loaded {
		c.count.Add(1)
		return
	}
	// Existing entry — overwrite without changing the count.
	c.m.Store(key, workDirResolveCacheEntry{
		resolved:  resolved,
		expiresAt: now.Add(workDirResolveCacheTTL),
	})
}

// workDirResolveCacheKey joins the three inputs with `\x00` (invalid in paths)
// so distinct triples cannot collide.
func workDirResolveCacheKey(workDir, allowedRoot, allowedRootResolved string) string {
	return workDir + "\x00" + allowedRoot + "\x00" + allowedRootResolved
}

// workDirResolveUnderRootCached is the Scheduler-scoped variant that memoises
// positive results in s.workDirCache. The pure workDirResolveUnderRoot stays
// the canonical correctness path: cold callers (loadJobs / UpdateJob) run once
// per operator action and a stale-cached resolve would mask a deliberate retarget.
func (s *Scheduler) workDirResolveUnderRootCached(workDir string) (string, bool) {
	if s == nil {
		return workDirResolveUnderRoot(workDir, "", "")
	}
	// s.now() (injectable clock) so the TTL boundary is testable without sleeping.
	now := s.now()
	// Precomputed suffix avoids the three-segment concat alloc each tick.
	key := workDir + s.workDirCacheKeySuffix
	if resolved, ok := s.workDirCache.lookup(key, now); ok {
		return resolved, true
	}
	resolved, ok := workDirResolveUnderRoot(workDir, s.allowedRoot, s.allowedRootResolved)
	if ok {
		s.workDirCache.store(key, resolved, now)
	}
	return resolved, ok
}

// workDirReachableCached is the Scheduler-scoped variant of workDirReachable
// that memoises positive results in s.workDirReachableCache (same TTL, keyed
// by raw workDir) so fresh-mode jobs do not os.Stat every tick. Negative
// results bypass the cache so a restored workspace is seen on the next tick.
// Empty workDir is always reachable and not cached (#1731).
func (s *Scheduler) workDirReachableCached(workDir string) bool {
	if s == nil || workDir == "" {
		return workDirReachable(workDir)
	}
	now := s.now()
	if _, ok := s.workDirReachableCache.lookup(workDir, now); ok {
		return true
	}
	if workDirReachable(workDir) {
		s.workDirReachableCache.store(workDir, "", now)
		return true
	}
	return false
}

// workDirResolveUnderRoot is the variant of workDirUnderRoot that also returns
// the symlink-resolved workDir, so callers hand the CLI the same path that was
// validated (closing the EvalSymlinks TOCTOU window). Empty workDir / root
// returns ("", true) so the caller leaves opts.Workspace untouched.
//
// SHARED-ALGORITHM-WITH-SERVER (#1316): the EvalSymlinks → resolve-root →
// prefix algorithm lives in osutil.ResolveWorkspaceUnderRoot, shared with the
// server's validateWorkspace so a containment fix cannot drift between the two
// call sites; cron keeps the (resolved, ok) shape, the server maps sentinel
// errors onto HTTP status codes.
func workDirResolveUnderRoot(workDir, allowedRoot, allowedRootResolved string) (string, bool) {
	return osutil.ResolveWorkspaceUnderRoot(
		workDir, allowedRoot, allowedRootResolved, filepath.EvalSymlinks)
}
