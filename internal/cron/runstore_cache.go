package cron

// This file was split out of runstore.go (#1282) via a pure move-only
// refactor: the recent-cache cluster (skipAppendTrim, appendTrimBatch,
// cacheHeadPush, cacheGet, warmCache, warmCacheLocked, cacheGetBefore,
// cacheInvalidate) was relocated verbatim. Same package (cron), same
// *runStore receivers, same jobLock / entry.mu critical sections and
// acquire/release order. No behaviour change.

import (
	"log/slog"
	"path/filepath"
	"time"
)

// skipAppendTrim returns true when the cache for jobID indicates the on-disk
// run count is comfortably below keepCount (with an appendTrimBatch headroom
// margin) and the keepWindow age policy won't have anything to evict yet
// (oldest cached row newer than cutoff). In that case Append's per-call
// trimJobLocked → ReadDir is pure overhead and is skipped. R232-PERF-8.
//
// Falls back to "do not skip" when the cache is cold or the safety margins
// don't hold — over-keeping a few entries is acceptable; missing a trim
// entirely is not.
//
// R20260527-PERF-24 (#1295) reworked the priority so the cache-headroom
// proofs (capSafe/windowSafe) decide the result directly. The earlier
// appendsSinceTrim "force a trim every appendTrimBatch Appends" counter is
// gone: when both proofs hold there is provably nothing to evict, so a
// forced periodic ReadDir would only ever find no work. R20260607-COR-002
// (#1901) removed the now-dead counter field and its bookkeeping.
//
// CALLER CONTRACT: caller MUST hold jobLock(jobID). Without jobLock
// serialisation a concurrent Append's cacheHeadPush (also jobLock-serialised)
// could race trimJobLocked against a fresh Append's WriteFileAtomic. Today
// the sole caller is Append (runstore.go:252) which acquires jobLock at line
// 213; any future helper must do the same. R239-GO-5.
func (s *runStore) skipAppendTrim(jobID string, now time.Time) bool {
	// Race-detector friendly contract assertion: panics when jobLock is
	// currently free, the unambiguous signature of a caller that forgot to
	// lock. False negatives accepted (another goroutine may hold the lock
	// while ours doesn't); false positives impossible.
	s.assertJobLockHeld(jobID)
	v, ok := s.recentCache.Load(jobID)
	if !ok {
		return false
	}
	entry := v.(*recentCacheEntry)
	entry.mu.RLock()
	defer entry.mu.RUnlock()
	if !entry.warm {
		return false
	}
	// R20260527-PERF-24 (#1295): the cache-headroom proofs below decide the
	// result directly. The prior implementation force-returned false on an
	// appendsSinceTrim boundary regardless of cap/window state, walking the
	// runs/<jobID>/ ReadDir+Stat tree every 10 Appends even when the cache
	// could prove no candidate exists (steady-state: 1run/min × 50 jobs × 30
	// days = 14400 wasted ReadDirs/day). appendTrimBatch survives only as the
	// capSafe headroom margin.
	// Plenty of headroom under count cap?  Cache reflects the on-disk
	// newest-first ring (capped to keepCount), so entry.count is a safe
	// upper bound on disk rows that survived the last trim.
	capSafe := entry.count+appendTrimBatch <= s.keepCount
	// Oldest cached row still inside keepWindow?  Use EndedAt to mirror
	// trimJobLocked's mtime-based cutoff (cacheTrimAfterDisk also approximates
	// mtime via EndedAt — keep these two paths consistent).
	windowSafe := true
	if entry.count > 0 {
		oldest := entry.ringRead(entry.count - 1)
		ts := oldest.EndedAt
		if ts.IsZero() {
			ts = oldest.StartedAt
		}
		cutoff := now.Add(-s.keepWindow)
		if !ts.After(cutoff) {
			windowSafe = false
		}
	}
	if capSafe && windowSafe {
		// Both cache-state proofs hold — nothing for trimJobLocked to do.
		return true
	}
	// One of the two cache proofs failed — there may be on-disk work for
	// trimJobLocked. Run it now.
	return false
}

// appendTrimBatch is the maximum number of Append calls we'll let pass
// without running trimJobLocked when skipAppendTrim's safety conditions
// hold. Picked low enough that even a runaway 1 Hz job sees a trim every
// 10 s.
const appendTrimBatch = 10

// cacheHeadPush prepends summary to the recentCache for jobID. The
// caller must hold jobLock(jobID) so the push is serialised against
// concurrent Recent / List reads. No-op on the ring when the cache
// entry is not yet warm — but we still LoadOrStore an empty placeholder
// so the next cacheGet avoids the redundant LoadOrStore + alloc on its
// own miss path. R246-GO-9 (#702): the pre-fix version returned silently
// when Load missed, leaving cacheGet to allocate the recentCacheEntry
// itself moments later.
//
// R242-GO-8 / R235-PERF-3 / R233-PERF-2 (#556): ring-buffer push in O(1).
// The pre-ring implementation did `append([]T{x}, slice...)` (later
// `append + copy + index`) which shifted up to keepCount-1 entries on
// every Append — at keepCount=200 that was 200× the per-push work the
// 1Hz cron + dashboard poll path actually needs. ringPushHead below is
// the O(1) implementation that landed via R243-PERF-4; #556 was the
// repeat finding before the cluster was wired up.
func (s *runStore) cacheHeadPush(jobID string, summary CronRunSummary) {
	// R242-ARCH-12 (#753): the jobLock contract was enforced only by godoc.
	// Mirror skipAppendTrim / trimJobLocked and run the best-effort runtime
	// check so a caller that forgot to hold jobLock surfaces in tests instead
	// of as a silent cache↔disk race. Gated by testing.Testing() inside the
	// helper, so production pays only the function-call overhead.
	s.assertJobLockHeld(jobID)
	v, ok := s.recentCache.Load(jobID)
	if !ok {
		// Lazy-allocate the placeholder so cacheGet doesn't have to. The
		// summary is NOT seeded into the placeholder ring: warm=false stays
		// because warmCache must still read disk to pick up records that
		// predate process start. Once warmCache lands, all subsequent
		// cacheHeadPush calls observe warm=true and push into the ring.
		actual, _ := s.recentCache.LoadOrStore(jobID, &recentCacheEntry{})
		v = actual
	}
	entry := v.(*recentCacheEntry)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !entry.warm {
		return
	}
	// Defensive: a warm cache must always own a cap=keepCount ring (warmCache
	// guarantees this via ringSeed). Re-allocate if a future caller bypasses
	// ringSeed; cheap and avoids index-out-of-range under unexpected input.
	if cap(entry.ring) != s.keepCount {
		entry.ringSeed(nil, s.keepCount)
	}
	// R20260527122801-PERF-4 (#1335): with WriteFileAtomic now hoisted out
	// of jobLock (so concurrent Appends do not serialise on the slow
	// fsync+rename), warmCache and Append's cacheHeadPush can interleave
	// such that warmCache reads the freshly-renamed file(s) from disk and
	// seeds them into the ring BEFORE the matching cacheHeadPush re-acquires
	// the lock to push. Without dedup, that interleaving would land the
	// same RunID twice in the ring. Head-only dedup is insufficient because
	// warmCache can seed multiple concurrently-written rows ahead of any of
	// their late-arriving pushes (e.g. ring [Y,X], then X's late push would
	// otherwise dup since head==Y).
	//
	// #1517: the dedup is now an O(1) map membership test against entry.runIDs
	// (maintained in lockstep with the ring by ringSeed / ringPushHead) instead
	// of the previous O(count) linear ring scan on every Append. Steady state
	// (1Hz × N jobs) drops from O(count) string comparisons per push to a
	// single map lookup. The runIDs index is allocated by ringSeed, which a
	// warm cache always ran first; guard against a nil index defensively.
	if entry.runIDs != nil {
		if _, dup := entry.runIDs[summary.RunID]; dup {
			return
		}
	}
	entry.ringPushHead(summary)
}

// cacheGet returns a defensive copy of up to limit newest summaries for
// jobID. Triggers a warm pass if the entry has not been hydrated yet.
// Caller must NOT hold jobLock — warmCache acquires it internally to
// populate the entry from disk.
//
// R240-GO-6 (#1039): a warm cache with count=0 is INTENTIONALLY treated as
// a hit (returns (nil, true)) — not a miss. Forcing a disk fallback on
// warm-empty would re-ReadDir on every List call for jobs that have never
// run, defeating the whole point of the cache. The "stale empty masks new
// disk row" race is foreclosed by a two-part contract:
//
//  1. warmCache holds jobLock while it ReadDirs and seeds the ring; Append
//     also holds jobLock around its cacheHeadPush. Neither side can read
//     a half-installed ring.
//  2. R20260527122801-PERF-4 (#1335) hoisted Append's WriteFileAtomic OUT
//     of jobLock so a concurrent warmCache CAN now ReadDir a fresh file
//     before the matching cacheHeadPush runs. cacheHeadPush dedups by
//     RunID against the ring head so the warmCache-then-push interleave
//     does not insert a duplicate. The "fresh disk row" still becomes
//     visible — either via warmCache's seed OR via cacheHeadPush — so
//     readers never miss it. Empty caches do not stay empty once a run
//     lands; they stay correct.
func (s *runStore) cacheGet(jobID string, limit int) ([]CronRunSummary, bool) {
	v, ok := s.recentCache.Load(jobID)
	if !ok {
		// Lazy-allocate the entry; warmCache will populate it.
		entry := &recentCacheEntry{}
		actual, _ := s.recentCache.LoadOrStore(jobID, entry)
		v = actual
	}
	entry := v.(*recentCacheEntry)
	entry.mu.RLock()
	if entry.warm {
		out := entry.ringSnapshot(limit)
		entry.mu.RUnlock()
		return out, true
	}
	entry.mu.RUnlock()

	// Cold cache: warm from disk under jobLock so concurrent Append's
	// cacheHeadPush observes the freshly-warmed ring (and would no-op
	// before, but warm is now true).
	//
	// Double-lock note: between the unlock above and the re-lock below
	// another goroutine may also enter cacheGet for this jobID and call
	// warmCache concurrently. warmCache is idempotent (entry.warm
	// transitions from false to true exactly once thanks to the per-job
	// lock guard), so the second caller sees warm=true on its own
	// re-acquire and returns the populated ring.
	//
	// R241-CR-6: warmCache always sets warm=true (even when ReadDir
	// fails or the directory is empty — diskListNewestFirst returns nil
	// and we cache the absence). The post-warm check below is therefore
	// a defensive guard against a future warmCache change rather than a
	// real disk-error fallback path.
	s.warmCache(jobID)
	// R247-GO-6 (#483): re-Load() after warmCache so a concurrent
	// cacheInvalidate (DeleteJob path) that races between our initial
	// LoadOrStore and warmCache's own LoadOrStore cannot leave us
	// reading the stale `entry` reference whose `warm=false` will never
	// be flipped — warmCache populated a DIFFERENT entry under the same
	// jobID. Without this re-Load the result was a silent permanent
	// (nil, false) miss until the next Append re-seeded the cache.
	if v2, ok := s.recentCache.Load(jobID); ok {
		entry = v2.(*recentCacheEntry)
	}
	entry.mu.RLock()
	defer entry.mu.RUnlock()
	if !entry.warm {
		return nil, false
	}
	return entry.ringSnapshot(limit), true
}

// warmCache populates the recentCache for jobID by reading the on-disk
// runs/<jobID>/ directory and parsing each .json file. Holds the per-job
// disk lock so a concurrent Append can't race the warm pass.
//
// Post-condition (R241-CR-6 / #486): on return, the cache entry's
// warm flag is true REGARDLESS of disk outcome — diskListNewestFirst
// folds ReadDir failures into a nil rows + 0 corruptCount return, and
// ringSeed installs an empty ring; warm=true is set unconditionally
// before the inner Unlock. This intentionally caches the absence of
// runs (or a transient disk error) for the jobLock+entry.mu window so
// a 1Hz dashboard poller does not re-ReadDir the same failing
// directory on every call. A subsequent Append always invalidates +
// re-warms via cacheHeadPush + warmCacheLocked, so a transient ENOENT
// during process startup self-heals on the first persisted run. The
// "post-warm `if !entry.warm { return nil, false }`" guard in cacheGet
// is therefore a defensive belt for a future warmCache change rather
// than a real disk-error fallback path.
//
// R236-PERF-09 (#527, partial): the corrupt-file slog.Warn was hoisted
// past lock release so a slow stderr / structured-log shipper can't
// extend the jobLock + entry.mu window that blocks concurrent Append
// and cacheGet. The slog cost is small in steady state but unbounded
// when the operator ships logs over a slow sink — keeping observability
// out of the lock window is cheaper than auditing every log handler.
func (s *runStore) warmCache(jobID string) {
	corruptCount, unreadableCount := s.warmCacheLocked(jobID)
	dir := filepath.Join(s.root, jobID)
	if corruptCount > 0 {
		slog.Warn("cron runstore warmCache skipped corrupt files",
			"count", corruptCount, "dir", dir)
	}
	// R20260603-CR-1 (#1693): log unreadable (EACCES/EIO/ESTALE) separately
	// from corrupt so operators can distinguish data loss from I/O errors.
	if unreadableCount > 0 {
		slog.Warn("cron runstore warmCache skipped unreadable files",
			"count", unreadableCount, "dir", dir)
	}
}

// warmCacheLocked is the inner critical section of warmCache. Returns
// separate counts of corrupt (ErrCorruptRun) and unreadable (other I/O
// error) run files so the caller can emit distinct aggregate slog messages
// AFTER the locks drop. R20260603-CR-1 (#1693).
// Callers MUST NOT hold any runStore lock; this function takes jobLock and
// entry.mu internally.
func (s *runStore) warmCacheLocked(jobID string) (corruptCount int, unreadableCount int) {
	lock := s.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()

	v, _ := s.recentCache.LoadOrStore(jobID, &recentCacheEntry{})
	entry := v.(*recentCacheEntry)

	// Fast warm check under entry.mu. jobLock already serialises warm passes
	// and cacheHeadPush against each other, so a concurrent warm cannot flip
	// entry.warm while we hold jobLock; the RLock here only reads warm safely
	// relative to in-flight cacheGet readers.
	entry.mu.RLock()
	alreadyWarm := entry.warm
	entry.mu.RUnlock()
	if alreadyWarm {
		return 0, 0 // another goroutine warmed it before we took jobLock
	}

	// R20260607-PERF-6 (#1903): do the ReadDir + per-file ReadFile WITHOUT
	// holding entry.mu. On FUSE/NFS the disk scan is a chain of network
	// round-trips; holding entry.mu across it would block every concurrent
	// cacheGet/cacheGetBefore RLock reader for the full latency. jobLock (held
	// for the whole function) already excludes other writers — concurrent
	// warmCacheLocked and cacheHeadPush both acquire jobLock first — so the
	// entry's ring/warm state cannot change underneath us while entry.mu is
	// released here. Readers that observe warm=false during the gap simply
	// fall back to disk (cacheGetBefore) or block on jobLock behind us
	// (cacheGet → warmCache), then see the seeded ring once we publish it.
	rows, corruptCount, unreadableCount := s.diskListNewestFirst(jobID, s.keepCount, time.Time{})

	// Re-acquire entry.mu only to publish the seeded ring. Re-check warm under
	// the lock as a defensive belt: jobLock guarantees no other warm ran, but
	// this keeps the helper correct even if a future refactor narrows the
	// jobLock window. If someone else already warmed, discard our scan.
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.warm {
		return 0, 0
	}
	entry.ringSeed(rows, s.keepCount)
	entry.warm = true
	return corruptCount, unreadableCount
}

// cacheGetBefore is the before-cutoff variant of cacheGet. It serves a
// before-filtered, newest-first slice from the cache only when the cache
// is provably exhaustive — i.e. cache.count < keepCount, meaning every
// on-disk row already lives in the ring and no entry has ever been
// trimmed off the tail. In that regime the cache holds strictly the
// same rows as a fresh disk scan, so a filter walk is correctness-
// equivalent to diskListNewestFirst at zero ReadDir+ReadFile cost.
//
// Returns ok=false when count == keepCount (the cache may have shed
// older entries via trim) — caller falls back to disk so pagination
// beyond the cache horizon still works. Cold cache paths are NOT warmed
// here: a cold-cache before-cutoff query is rare (dashboard typically
// drives warm via a no-cutoff first page), so paying the warm cost on
// the pagination path would add a ReadDir+per-file ReadFile to a query
// that is already going to disk and reading it twice — once for warm,
// once via the disk fallback. The warm path lazy-warms on the next
// no-cutoff List call. R243-PERF-5 (#810).
//
// Caller must guard before.IsZero() == false; use cacheGet for the
// no-cutoff fast path.
func (s *runStore) cacheGetBefore(jobID string, limit int, before time.Time) ([]CronRunSummary, bool) {
	v, ok := s.recentCache.Load(jobID)
	if !ok {
		return nil, false
	}
	entry := v.(*recentCacheEntry)
	entry.mu.RLock()
	defer entry.mu.RUnlock()
	if !entry.warm {
		return nil, false
	}
	// Exhaustive only when cache hasn't hit cap. count == keepCount
	// means trimJobLocked may have evicted older rows that match the
	// before cutoff; disk scan is the safe answer.
	if entry.count >= s.keepCount {
		return nil, false
	}
	out := make([]CronRunSummary, 0, limit)
	for i := 0; i < entry.count; i++ {
		r := entry.ringRead(i)
		// diskListNewestFirst applies StartedAt strict-less-than the
		// cutoff; mirror that here so cache and disk paths stay in
		// lockstep on the equality boundary.
		if !before.IsZero() && !r.StartedAt.Before(before) {
			continue
		}
		out = append(out, r)
		// R20260606-PERF-11: break as soon as limit is reached rather than
		// continuing to scan the ring. The outer loop condition checked
		// len(out)<limit only at iteration start, so without this break we
		// would execute one extra ringRead after the slice is full.
		if len(out) >= limit {
			break
		}
	}
	return out, true
}

// cacheInvalidate forgets the cache entry for jobID. Used by DeleteJob.
func (s *runStore) cacheInvalidate(jobID string) {
	s.recentCache.Delete(jobID)
}
