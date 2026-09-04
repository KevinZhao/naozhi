package cron

import (
	"log/slog"
	"path/filepath"
	"time"
)

// skipAppendTrim returns true when the cache proves Append's trimJobLocked
// would find nothing: the entry is warm, count + appendTrimBatch headroom is
// under keepCount, and the oldest cached row is newer than the keepWindow
// cutoff. Falls back to "do not skip" when the cache is cold or a margin
// fails — over-keeping a few entries is acceptable; missing a trim is not.
//
// Caller MUST hold jobLock(jobID): every entry writer (cacheHeadPush,
// warmCacheLocked, cacheTrimAfterDisk) holds it, which is why entry.mu is
// taken in READ mode here and does not block concurrent dashboard readers.
func (s *runStore) skipAppendTrim(jobID string, now time.Time) bool {
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
	// Cache reflects the on-disk newest-first ring (capped to keepCount), so
	// entry.count is a safe upper bound on disk rows that survived the last trim.
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
	return false
}

// appendTrimBatch is the maximum number of Append calls we'll let pass
// without running trimJobLocked when skipAppendTrim's safety conditions
// hold. Picked low enough that even a runaway 1 Hz job sees a trim every
// 10 s.
const appendTrimBatch = 10

// cacheHeadPush prepends summary to the recentCache for jobID in O(1) via the
// ring. The caller must hold jobLock(jobID) so the push is serialised against
// concurrent Recent / List reads. When the entry is not yet warm the ring is
// left alone (warmCache must still read disk to pick up pre-start records)
// but an empty placeholder is still LoadOrStore'd so cacheGet's miss path
// does not allocate it again (#702).
func (s *runStore) cacheHeadPush(jobID string, summary CronRunSummary) {
	s.assertJobLockHeld(jobID)
	v, ok := s.recentCache.Load(jobID)
	if !ok {
		// The summary is NOT seeded into the placeholder: warm=false stays until
		// warmCache reads disk, after which pushes land in the ring.
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
	// Dedup by RunID: the disk write happens outside jobLock (#1335), so
	// warmCache can seed freshly-renamed files into the ring BEFORE the matching
	// cacheHeadPush re-acquires the lock; head-only dedup is insufficient because
	// several concurrently-written rows can be seeded ahead of their late pushes.
	// entry.runIDs is maintained in lockstep by ringSeed / ringPushHead (#1517).
	if entry.runIDs != nil {
		if _, dup := entry.runIDs[summary.RunID]; dup {
			return
		}
	}
	entry.ringPushHead(summary)
}

// cacheGet returns a defensive copy of up to limit newest summaries for
// jobID. Triggers a warm pass if the entry has not been hydrated yet.
// Caller must NOT hold jobLock — warmCache acquires it internally.
//
// A warm cache with count=0 is INTENTIONALLY a hit (nil, true), not a miss:
// a disk fallback on warm-empty would re-ReadDir on every List for jobs that
// never ran (#1039). A fresh disk row cannot be masked: warmCache and
// cacheHeadPush both run under jobLock, and cacheHeadPush dedups by RunID, so
// the row becomes visible via one of the two paths.
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

	// Cold cache: warm from disk under jobLock. A concurrent cacheGet may call
	// warmCache too; it is idempotent (warm flips false→true once under the
	// per-job lock). warmCache always sets warm=true even on ReadDir failure or
	// an empty dir, so the post-warm `!entry.warm` check below is a defensive
	// belt, not a real disk-error fallback.
	s.warmCache(jobID)
	if s.cacheGetPostWarmHook != nil {
		s.cacheGetPostWarmHook(jobID)
	}
	// Re-Load after warmCache: a concurrent cacheInvalidate (DeleteJob) racing
	// between our LoadOrStore and warmCache's own LoadOrStore would otherwise
	// leave us reading a stale entry whose warm=false never flips (#483).
	v2, ok := s.recentCache.Load(jobID)
	if !ok {
		// Key vanished between warmCache and this re-Load: a concurrent DeleteJob
		// invalidated it. Serving the pre-warm pointer could return rows of a job
		// being deleted, so a miss is the correct answer (#2000).
		return nil, false
	}
	entry = v2.(*recentCacheEntry)
	entry.mu.RLock()
	defer entry.mu.RUnlock()
	if !entry.warm {
		return nil, false
	}
	return entry.ringSnapshot(limit), true
}

// warmCache populates the recentCache for jobID from runs/<jobID>/ under the
// per-job lock so a concurrent Append cannot race the warm pass.
//
// Post-condition: the entry's warm flag is true REGARDLESS of disk outcome
// (ReadDir failure → nil rows + empty ring). This caches the absence of runs
// or a transient error so a 1Hz poller does not re-ReadDir a failing dir;
// the next Append re-warms via cacheHeadPush + warmCacheLocked (#486). The
// slog.Warn calls sit outside the lock window so a slow log sink cannot
// extend the jobLock + entry.mu hold (#527).
func (s *runStore) warmCache(jobID string) {
	corruptCount, unreadableCount := s.warmCacheLocked(jobID)
	dir := filepath.Join(s.root, jobID)
	if corruptCount > 0 {
		slog.Warn("cron runstore warmCache skipped corrupt files",
			"count", corruptCount, "dir", dir)
	}
	// Unreadable (EACCES/EIO/ESTALE) logged separately from corrupt (#1693).
	if unreadableCount > 0 {
		slog.Warn("cron runstore warmCache skipped unreadable files",
			"count", unreadableCount, "dir", dir)
	}
}

// warmCacheLocked is the inner critical section of warmCache. Returns
// separate corrupt (ErrCorruptRun) and unreadable (other I/O error) counts so
// the caller can log AFTER the locks drop (#1693). Callers MUST NOT hold any
// runStore lock; this function takes jobLock and entry.mu internally.
func (s *runStore) warmCacheLocked(jobID string) (corruptCount int, unreadableCount int) {
	lock := s.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()

	v, _ := s.recentCache.LoadOrStore(jobID, &recentCacheEntry{})
	entry := v.(*recentCacheEntry)

	// jobLock already serialises warm passes and cacheHeadPush, so warm cannot
	// flip while we hold it; the RLock only orders the read against cacheGet readers.
	entry.mu.RLock()
	alreadyWarm := entry.warm
	entry.mu.RUnlock()
	if alreadyWarm {
		return 0, 0 // another goroutine warmed it before we took jobLock
	}

	// ReadDir + per-file ReadFile run WITHOUT entry.mu: on FUSE/NFS the scan is a
	// chain of round-trips and would block every cacheGet reader. jobLock (held
	// for the whole function) already excludes the other writers, so ring/warm
	// state cannot change underneath us; readers seeing warm=false fall back to
	// disk or queue behind jobLock (#1903).
	rows, corruptCount, unreadableCount := s.diskListNewestFirst(jobID, s.keepCount, time.Time{})

	// Re-acquire entry.mu only to publish. Re-check warm as a defensive belt in
	// case a future refactor narrows the jobLock window.
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.warm {
		return 0, 0
	}
	entry.ringSeed(rows, s.keepCount)
	entry.warm = true
	return corruptCount, unreadableCount
}

// cacheGetBefore is the before-cutoff variant of cacheGet. It serves from the
// cache only when the cache is provably exhaustive — count < keepCount, so no
// row has ever been trimmed off the tail and a filter walk equals a disk scan.
// Returns ok=false at count == keepCount so pagination beyond the cache
// horizon falls back to disk. A cold cache is NOT warmed here: a cold
// before-cutoff query is rare and would read the dir twice (warm + fallback);
// the next no-cutoff List lazy-warms (#810).
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
	// count == keepCount means trim may have evicted rows matching the cutoff;
	// disk is the safe answer.
	if entry.count >= s.keepCount {
		return nil, false
	}
	// Lazily allocate out so empty-job / all-filtered hits return nil without the make.
	var out []CronRunSummary
	for i := 0; i < entry.count; i++ {
		r := entry.ringRead(i)
		// Mirror diskListNewestFirst's strict StartedAt < before on the equality boundary.
		if !before.IsZero() && !r.StartedAt.Before(before) {
			continue
		}
		if out == nil {
			out = make([]CronRunSummary, 0, limit)
		}
		out = append(out, r)
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
