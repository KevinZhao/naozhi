package cron

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// workDirReachable reports whether workDir exists and resolves to a
// directory right now. Used before fresh-mode Reset so a job whose
// workspace has been deleted by an operator does not destroy the
// existing session just to fail on a GetOrCreate / spawn-shim call.
// Empty workDir means "use router default" and is always reachable.
// CRON2.
//
// 注意：workDirReachable 仅做 stat 可达性 + IsDir 检查，**不**强制
// allowedRoot 内含。任何依赖"必须在工作根之内"的调用者必须额外调
// workDirUnderRoot。当前调用点 (freshContextPreflightP0) 依赖
// loadJobs 阶段已做过 root-containment 校验；不要在不调
// workDirUnderRoot 的新调用点直接复用本函数。R234-CR-11。
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
// to a path at or under allowedRoot. EvalSymlinks is done per-call for both
// sides so the check reflects current filesystem state — this closes the
// TOCTOU window between creation-time validateWorkspace and execute-time
// workspace binding AND the separate window where allowedRoot itself (if a
// symlink) could be retargeted after construction. Both arguments must be
// absolute; relative workDir is rejected. allowedRootResolved, when
// non-empty, is a best-effort prior resolution of allowedRoot that is used
// as a fallback only if the per-call EvalSymlinks on allowedRoot itself
// fails (e.g. the path was temporarily unmounted). This preserves the
// security contract while still avoiding most of the syscall cost of a
// cold re-resolution on the happy path.
func workDirUnderRoot(workDir, allowedRoot, allowedRootResolved string) bool {
	_, ok := workDirResolveUnderRoot(workDir, allowedRoot, allowedRootResolved)
	return ok
}

// workDirResolveCacheTTL caps how long a positive workDirResolveUnderRoot
// result may be reused before re-running EvalSymlinks. R247-PERF-24
// (#572): long-lived schedulers re-evaluate the same per-job workDir every tick;
// each call costs Lstat+Readlink per path component plus the same chain
// for allowedRoot. A short TTL collapses the hot-path syscall load on
// fast-firing jobs while still bounding the TOCTOU window the per-call
// EvalSymlinks was added to close. 30s matches the cronNotifyTimeout
// budget — an operator who retargets a workspace symlink and immediately
// fires a job will see the next tick re-resolve, not the same tick. Only
// "ok" results are cached: a negative answer means we just refused to
// run, and we want a re-resolve on the next call to surface a workspace
// that has been restored.
const workDirResolveCacheTTL = 30 * time.Second

// workDirResolveCacheEntry captures one cached resolution. Stored value-
// typed in sync.Map so the read path does no allocation.
type workDirResolveCacheEntry struct {
	resolved  string
	expiresAt time.Time
}

// workDirResolveCacheMaxEntries caps the number of resolved (workDir,
// allowedRoot, allowedRootResolved) tuples retained in memory. A workDir
// is operator-controlled (cron job WorkDir) and entries expire only on
// read of the same key. Without a cap a buggy or hostile job-creation
// path that varies WorkDir slightly per call (e.g. trailing slash, NFC vs
// NFD, /./ insertion) would grow the map indefinitely. R20260527-SEC-4
// (#1273): once the cap is hit, store sweeps expired entries first; if
// that fails to free room (every entry still within TTL), the new write
// is dropped — the cache is a hot-path optimisation, not a correctness
// path, so missing a cache slot just defers to workDirResolveUnderRoot.
//
// 4096 sized to comfortably exceed defaultMaxJobs (256) × a few distinct
// allowedRoots even with restart-time churn, while bounding worst-case
// memory at ~1.5 MB (avg key+value ~ 384 bytes).
const workDirResolveCacheMaxEntries = 4096

// workDirResolveCache memoises positive workDirResolveUnderRoot results
// keyed by raw (workDir,allowedRoot,allowedRootResolved) tuple. Negative
// results bypass the cache. Concurrent-safe via sync.Map; entries expire
// lazily on read so a wedged job does not pin stale resolutions
// indefinitely. R247-PERF-24.
type workDirResolveCache struct {
	m     sync.Map     // map[string]workDirResolveCacheEntry
	count atomic.Int64 // approximate live entries; allowed to drift slightly
}

// nowFn is overridable for tests so the TTL boundary can be exercised
// deterministically. Production always reads time.Now.
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
		// Expired — drop so the next miss path doesn't keep observing it.
		// LoadAndDelete keeps c.count in sync iff the entry was actually
		// present (concurrent expirers won't double-decrement).
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
	// Cap enforcement: when the map is at or above the cap, sweep expired
	// entries first to make room. If sweep didn't free anything (every
	// entry still warm), drop the new write — the cache is a perf
	// optimisation; missing a slot only costs one extra
	// workDirResolveUnderRoot call on the next tick.
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

// workDirResolveCacheKey concatenates the three inputs with separators
// that are not valid in absolute paths (`\x00`) so distinct triples
// cannot collide on a single key. R247-PERF-24.
func workDirResolveCacheKey(workDir, allowedRoot, allowedRootResolved string) string {
	return workDir + "\x00" + allowedRoot + "\x00" + allowedRootResolved
}

// workDirResolveUnderRoot is the variant of workDirUnderRoot that also
// returns the symlink-resolved workDir on success. R246-GO-12: callers
// that subsequently hand workDir to a CLI (cli wrapper / claude spawn)
// should use the resolved path so the open-time view matches the
// validation-time view. Without this the workDir we just validated may
// resolve differently when the CLI re-runs EvalSymlinks (TOCTOU window),
// re-introducing the symlink-swap escape that EvalSymlinks-on-validate
// was meant to close.
//
// Returned path is filepath.Clean'd (EvalSymlinks already does that).
// On the empty-workDir / empty-root short-circuit returns ("", true)
// so the caller leaves opts.Workspace untouched (router default applies).
//
// SHARED-ALGORITHM-WITH-SERVER (R20260527122801-ARCH-4 / #1316): the
// EvalSymlinks(workDir) → EvalSymlinks(allowedRoot) → equality-or-prefix
// algorithm has been hoisted to osutil.ResolveWorkspaceUnderRoot, so cron and
// the server's validateWorkspace now share one canonical containment + resolve
// path. workDirResolveUnderRoot below is a thin adapter that keeps cron's
// (resolved, ok) shape (the dispatcher treats both "rejected" and "no
// constraint" as "leave opts.Workspace untouched"); the server wraps the same
// helper to map sentinel errors onto HTTP status codes. A fix to the
// containment rule now lands in osutil and cannot silently drift between the
// two call sites.
// workDirResolveUnderRootCached is the Scheduler-scoped variant that
// memoises positive results in s.workDirCache. The pure
// workDirResolveUnderRoot below stays the canonical correctness path —
// cold callers (loadJobs / UpdateJob) keep using it because they run
// once per operator action and a stale-cached resolve would mask a
// deliberate retarget. R247-PERF-24.
func (s *Scheduler) workDirResolveUnderRootCached(workDir string) (string, bool) {
	if s == nil {
		return workDirResolveUnderRoot(workDir, "", "")
	}
	now := time.Now()
	// R20260526-PERF-002 (#1225): use precomputed suffix to avoid the
	// three-segment concat allocation each tick. Equivalent to
	// workDirResolveCacheKey(workDir, s.allowedRoot, s.allowedRootResolved).
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
// that memoises positive results in s.workDirReachableCache so fresh-mode
// jobs do not repeat the bare os.Stat every tick. Mirrors
// workDirResolveUnderRootCached: positive-only caching with the same
// workDirResolveCacheTTL, keyed by raw workDir. A negative result bypasses
// the cache so an operator who restores a deleted workspace sees the next
// tick re-stat rather than a pinned "unreachable". Empty workDir is always
// reachable and is not cached (workDirReachable short-circuits it).
// R20260604064416-PERF-3 (#1731).
func (s *Scheduler) workDirReachableCached(workDir string) bool {
	if s == nil || workDir == "" {
		return workDirReachable(workDir)
	}
	now := time.Now()
	if _, ok := s.workDirReachableCache.lookup(workDir, now); ok {
		return true
	}
	if workDirReachable(workDir) {
		s.workDirReachableCache.store(workDir, "", now)
		return true
	}
	return false
}

// workDirResolveUnderRoot delegates the EvalSymlinks → resolve-root → prefix
// algorithm to osutil.ResolveWorkspaceUnderRoot. R20260527122801-ARCH-4 (#1316):
// previously this function open-coded the same algorithm as the server's
// validateWorkspace; both copies could drift and silently re-open the
// symlink-swap escape on whichever side a fix missed. The shared helper is now
// the single canonical containment + resolve path; cron keeps the (resolved,
// ok) shape here (dispatcher treats both "rejected" and "no constraint" as
// "leave opts.Workspace untouched") while the server layer wraps the same
// helper to map sentinel errors onto HTTP status codes.
func workDirResolveUnderRoot(workDir, allowedRoot, allowedRootResolved string) (string, bool) {
	return osutil.ResolveWorkspaceUnderRoot(
		workDir, allowedRoot, allowedRootResolved, filepath.EvalSymlinks)
}
