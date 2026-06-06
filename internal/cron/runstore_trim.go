package cron

// This file was split out of runstore.go (#1282) via a move-only refactor
// (test-anchor-relocation): the trim/GC cluster (trimJobLocked,
// trimSkipFromCache, cacheTrimAfterDisk, trimAll, trimAllCtx,
// warmJobsParallel, trimJobUnderLock) was relocated verbatim. Same package
// (cron), same *runStore receivers, same jobLock / entry.mu critical
// sections and acquire/release order — including trimJobLocked's
// unlock-during-remove window. No behaviour change.

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// trimJobLocked enforces the per-job retention policy. Caller must hold
// jobLock(jobID) — the "Locked" suffix encodes the lock-already-held
// contract. now is passed in so tests can inject deterministic "current
// time" without touching time.Now().
//
// LOCKING (R236-GO-03): the contract used to live only in the comment
// above. The function performs ReadDir + os.Remove on the per-job runs
// directory and pushes the trimmed slice into the recentCache entry,
// which races concurrent Append (also holding jobLock) and concurrent
// cacheGet (holds entry.mu only) without serialisation if the caller
// failed to acquire jobLock. assertJobLockHeld below is a best-effort
// runtime guard: a successful TryLock means no goroutine — including
// the caller — was holding the lock, which is exactly the contract
// violation we want to catch in tests. False negatives are possible
// (another goroutine may hold it instead of the caller), but the most
// likely failure mode is "caller forgot to acquire" and that surfaces
// reliably in single-flight test scenarios.
//
// R20260527-COR-4 (#1291) "Locked" suffix is NOT continuous-hold:
// the os.Remove batch path below releases jobLock for slow-FS syscall
// fan-out (R246-GO-20 / #712) and re-acquires it before the cache
// reconciliation. The Locked suffix means "caller must hold on entry
// AND on exit" — interior windows may release. Append's deferred
// unlock contract still works because the Locked promise is restored
// before this function returns. A panic inside the unlocked window
// would surface as Append's deferred Unlock-on-non-held-lock panic
// (we deliberately do NOT add a deferred re-acquire — sync.Mutex is
// not re-entrant, so a re-acquire-on-panic would deadlock the panic
// goroutine on its own resumption against the original recovered
// caller). os.Remove on the std lib never panics on POSIX, so the
// fail-loud path is acceptable.
//
// Policy: keep ALL runs satisfying BOTH (rank ≤ keepCount) AND
// (age ≤ keepWindow). Either condition violated → delete. AND-vs-OR
// is the user-confirmed choice in the RFC chat (§4.3): high-frequency
// jobs get capped by count; low-frequency jobs by window.
func (s *runStore) trimJobLocked(jobID string, now time.Time) {
	s.assertJobLockHeld(jobID)
	// R236-PERF-12 (#532): cache-driven fast exit. trimJobLocked is invoked
	// from Append's appendTrimBatch boundary path (every N appends as a
	// background-drift safety net) AND from the cold-start trimAll pass.
	// When the cache is warm and shows count strictly below keepCount, the
	// cache enumerates every on-disk file (warmCache + jobLock-serialised
	// Append guarantee that invariant). Combined with cache's oldest
	// StartedAt being newer than the keepWindow cutoff, no file can possibly
	// need removal, so the ReadDir + per-entry Stat in scanSortedRunDir is
	// pure overhead. assertJobLockHeld above + entry.mu below prevent races
	// with cacheHeadPush / Append. The check is purely additive: any path
	// that would have entered the ReadDir branch still does (a cold cache
	// or count >= keepCount falls through). Cache-trim reconciliation
	// (cacheTrimAfterDisk) is intentionally skipped here — there was nothing
	// to remove, so the cache is already in sync with disk.
	if s.trimSkipFromCache(jobID, now) {
		return
	}
	// R239-PERF-5 (#871): scan + sort delegated to scanSortedRunDir so the
	// trim path is in lockstep with diskListNewestFirst (matching sort
	// order, symlink filter, IsValidID guard) and the duplicate ReadDir +
	// Stat-per-entry — expensive on FUSE/NFS — runs through one shared
	// implementation rather than two open-coded copies that drifted on
	// every R220/R222/R235/R236 review.
	items, _, err := s.scanSortedRunDir(jobID)
	if err != nil {
		return
	}
	cutoff := now.Add(-s.keepWindow)
	// items already sorted newest first (mtime DESC). Walk once to detect
	// any expired entry — the first one not after cutoff means every
	// later entry is also expired, so we can break early. The boolean
	// feeds the "under-cap AND no expiry" fast path that returns without
	// running the remove loop below.
	anyExpired := false
	for _, it := range items {
		if !it.mtime.After(cutoff) {
			anyExpired = true
			break
		}
	}
	// Fast path: under cap AND nothing expired → no remove. The common
	// case for healthy 5-min cron jobs that ride well under the 200-entry
	// cap. R220-PERF-3.
	if len(items) <= s.keepCount && !anyExpired {
		return
	}
	if len(items) == 0 {
		return
	}
	// R246-GO-20 (#712): collect the to-remove paths first under jobLock,
	// then release the lock for the os.Remove syscall batch so concurrent
	// Append for the same jobID can proceed during slow-FS removes (FUSE/
	// NFS can take 10s of ms per Remove). Re-acquire jobLock before
	// cacheTrimAfterDisk so the cache reconciliation stays serialised
	// against cacheHeadPush as before.
	//
	// Safety: the snapshot is captured under lock so a concurrent Append
	// landing during the unlocked window writes a fresh file with newer
	// mtime which is by definition not in our toRemove slice — we never
	// delete it. The fresh Append's cacheHeadPush also runs after our
	// release, so the cache observes the new row before our reconcile;
	// cacheTrimAfterDisk preserves rows that survived the cutoff (it
	// counts survivors from the head, never reaching the new entry).
	//
	// items sorted newest first by scanSortedRunDir, so rank checking
	// below is index-based. Sort policy / tie-break rationale lives on
	// scanSortedRunDir's godoc; trim and list paths must observe the same
	// total order or the cap cutoff (i < keepCount) and the list cutoff
	// (StartedAt < before) disagree about which equal-mtime record to
	// drop. R235-GO-7 / R236-QA-01.
	//
	// R20260603-PERF-12: size the slice for the common case (a healthy job
	// removes only a handful of expired runs) rather than len(items) (up to
	// keepCount=200). append's growth strategy handles the rare bulk-trim.
	toRemove := make([]string, 0, 4)
	for i, it := range items {
		// Both conditions must hold to keep.
		keep := i < s.keepCount && it.mtime.After(cutoff)
		if keep {
			continue
		}
		toRemove = append(toRemove, it.path)
	}
	if len(toRemove) > 0 {
		// R20260527-GO-9 (#1271): wrap the unlock window so the inner
		// re-Lock fires from a deferred call. If os.Remove panics (FUSE
		// quirks, syscall traps), the bare `lock.Lock()` after the loop
		// would never execute, the function unwinds with the per-job lock
		// in the UNLOCKED state, and the outer caller's defer Unlock then
		// panics on "unlock of unlocked mutex" — masking the original
		// trigger and corrupting the lock state for every subsequent
		// Append on the same jobID. Wrapping the unlock window in a
		// closure with `defer lock.Lock()` guarantees the lock is
		// re-acquired before any panic propagates, preserving the
		// outer caller's lock-held contract.
		lock := s.jobLock(jobID)
		func() {
			lock.Unlock()
			defer lock.Lock()
			for _, p := range toRemove {
				if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
					slog.Debug("cron run: trim remove failed", "path", p, "err", err)
				}
			}
		}()
	}
	// Cache may now point to deleted entries; reconcile by trimming the
	// cache slice to the same (count + window) policy. We hold jobLock so
	// concurrent Append's cacheHeadPush can't race.
	s.cacheTrimAfterDisk(jobID, cutoff)
}

// trimSkipFromCache reports whether the cache state proves trimJobLocked
// has no work to do, allowing the caller to skip ReadDir + per-entry Stat.
// Returns true only when ALL of:
//
//   - cache is warm (so count + ring entries are authoritative);
//   - cache.count < s.keepCount (so cache enumerates every on-disk file —
//     a count == keepCount means trim may have shed older entries beyond
//     the ring horizon and we can no longer be sure no extra file lingers);
//   - oldest cached row's timestamp is strictly newer than the cutoff (so
//     no window-based eviction is due).
//
// Caller MUST hold jobLock(jobID); the entry.mu acquisition here pairs
// with cacheHeadPush / cacheTrimAfterDisk so a concurrent Append's
// metadata mutation cannot race the read. R236-PERF-12 (#532).
//
// R242-PERF-10 (#674) is the same root and is closed by this fast path:
// trimJobLocked.scanSortedRunDir is bypassed entirely when the cache can
// prove no candidate exists. The proposal "derive trim decision from
// cache length" is realised by the count + oldest-row checks above —
// length is necessary but not sufficient because a warm cache with
// count==keepCount could still hide expired older rows on disk; we keep
// the strict "<keepCount" guard for safety.
func (s *runStore) trimSkipFromCache(jobID string, now time.Time) bool {
	// R242-ARCH-12 (#753): runtime-enforce the documented jobLock contract
	// (best-effort, test-only) so the lock hierarchy stops living solely in
	// godoc. See cacheHeadPush.
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
	// keepCount-margin: at count == keepCount the cache may have rotated
	// older entries off the ring (they could still exist on disk because
	// the previous trim might have failed or be pending). Stay strict.
	if entry.count >= s.keepCount {
		return false
	}
	// Empty cache → nothing on disk → nothing to trim. The trimJobLocked
	// scanSortedRunDir branch returns at len(items) == 0, but skipping the
	// syscall entirely is cheaper.
	if entry.count == 0 {
		return true
	}
	cutoff := now.Add(-s.keepWindow)
	oldest := entry.ringRead(entry.count - 1)
	ts := oldest.EndedAt
	if ts.IsZero() {
		ts = oldest.StartedAt
	}
	// Strict After: equal timestamps fall back to disk to avoid an off-by-one
	// "boundary mtime gets evicted by the disk path but not the cache path"
	// drift between trimJobLocked's mtime + cache's StartedAt approximation.
	if !ts.After(cutoff) {
		return false
	}
	return true
}

// cacheTrimAfterDisk reconciles the recentCache for jobID after on-disk
// trimJobLocked removed expired / over-cap entries. Called by trimJobLocked
// only — caller holds jobLock(jobID).
//
// Allocation contract (R241-PERF-6 / #480): trims in place on the existing
// ring backing array — no fresh `keep` slice. The pre-ring implementation
// rebuilt a `keep := []CronRunSummary{...}` slice every call; with the
// post-R221-FIX-P0-2 ring, ringSnapshot already returns fresh copies for
// readers, so this path mutates entry.head / entry.count + zeroes dropped
// slots without ever allocating. Hot-path zero-alloc property is load-
// bearing: trimJobLocked fires from Append's appendTrimBatch boundary
// (every 10 Appends) and the dashboard 1Hz dispatch path, so a per-call
// alloc would amplify GC pressure linearly with cron tick rate.
func (s *runStore) cacheTrimAfterDisk(jobID string, cutoff time.Time) {
	// R242-ARCH-12 (#753): runtime-enforce the documented jobLock contract
	// (best-effort, test-only) so the lock hierarchy stops living solely in
	// godoc. See cacheHeadPush.
	s.assertJobLockHeld(jobID)
	v, ok := s.recentCache.Load(jobID)
	if !ok {
		return
	}
	entry := v.(*recentCacheEntry)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !entry.warm {
		return
	}
	// Drop entries beyond keepCount; drop entries older than cutoff. The
	// ring is logically newest-first, so iterate via ringRead and stop at
	// the first row older than cutoff.
	//
	// The cutoff is mtime-based (matches trimJobLocked which uses ModTime
	// from os.DirEntry.Info). For long-running jobs StartedAt can predate
	// mtime by hours, so using StartedAt here would evict cache rows whose
	// disk files are still kept — leaving the 1Hz dashboard list endpoint
	// looking at fewer rows than disk holds until the next warmCache. We
	// approximate mtime via EndedAt (the rename happens immediately after
	// finishRun marshals the record), with StartedAt as the fallback for
	// the very rare in-progress snapshot where EndedAt is zero. R221-FIX-P1-3.
	//
	// R230B-CR-4: this approximation is the source of the "disk vs cache
	// time-source divergence" review note. trimJobLocked uses mtime
	// (resolution: filesystem rename time, ~ms after finishRun); cache
	// uses EndedAt (Go-level time.Now() at finishRun entry, ~ms before
	// rename). Divergence window: < ~10 ms typical, < ~1 s pathological.
	// For a long-running job that finishes near the keepWindow boundary
	// the cache row may be evicted ~ms before the disk row in the
	// extreme case; the next 1Hz poll re-warms from disk so the gap is
	// invisible to operators. Aligning both paths to mtime would require
	// either an os.Stat per cache row (250 syscall/s on the dashboard
	// path — see R232-PERF-3) or storing mtime alongside EndedAt
	// (cache size +8B per row × keepCount=200 jobs × 200 rows = 320KB).
	// Neither cost is justified for the observed divergence; godoc-only
	// resolution stands until profile data shows otherwise.
	//
	// R221-FIX-P0-2 historical: pre-ring this rebuilt a fresh slice every
	// trim because reusing entry.runs[:0] would alias snapshots taken by
	// concurrent List/Recent callers. With the ring, snapshots are made
	// via ringSnapshot which already returns fresh slices, so we trim in
	// place by counting how many rows survive the cutoff and writing back.
	limit := s.keepCount
	if limit > entry.count {
		limit = entry.count
	}
	// R249-PERF-9 (#930): the ring is newest-first by ts and the
	// `ts.Before(cutoff)` predicate is monotone (once a row is older than
	// cutoff every later row in the ring is also older), so the survive
	// boundary is exactly the smallest index i where ringRead(i).ts is
	// before cutoff — sort.Search territory. Linear scan walks up to
	// keepCount=200 rows on every Append-driven trim; binary search
	// collapses that to ~log2(200) ≈ 8 ringRead calls. Cosmetic cost
	// (handleList runs at 1Hz × N tabs and trim fires once per Append),
	// but the cleaner shape also documents the monotonicity contract
	// future readers need to preserve when changing the cutoff predicate.
	survive := sort.Search(limit, func(i int) bool {
		r := entry.ringRead(i)
		ts := r.EndedAt
		if ts.IsZero() {
			ts = r.StartedAt
		}
		return ts.Before(cutoff)
	})
	// R249-CR-19 (#962): record how many rows this approximate-time-source
	// trim evicted so operators can watch the divergence the godoc above
	// documents. survive < entry.count means the cache dropped rows based on
	// EndedAt/StartedAt rather than disk mtime; a growing delta vs disk-side
	// trims is the signal the approximation is mispredicting.
	if survive < entry.count {
		s.cacheStaleEvictionTotal.Add(int64(entry.count - survive))
	}
	// Zero out the dropped slots to release any retained string fields.
	// R20260602190132-PERF-10: replace the O(evicted) per-slot modulo loop
	// with at most two contiguous clear() calls that follow the same
	// branch-on-wrap pattern used by ringSnapshot/ringPushHead.
	//
	// Physical layout: logical slot i lives at ring[(head+i)%c].
	// The evicted logical range [survive, count) maps to a physical run
	// starting at phyStart = (head+survive)%c spanning numEvicted slots.
	// If phyStart+numEvicted <= c the run fits in one segment; otherwise it
	// wraps and requires two segments.
	c := cap(entry.ring)
	if c > 0 && survive < entry.count {
		// R20260604-CR-7: delete evicted RunIDs from the dedup index before
		// zeroing the ring slots. ringRead uses logical indices, so we must
		// call it while entry.count still equals the pre-trim value (before
		// the entry.count = survive assignment below). ringPushHead mirrors
		// this with the same nil guard.
		if entry.runIDs != nil {
			for i := survive; i < entry.count; i++ {
				delete(entry.runIDs, entry.ringRead(i).RunID)
			}
		}
		numEvicted := entry.count - survive
		phyStart := (entry.head + survive) % c
		if phyStart+numEvicted <= c {
			// Single contiguous segment — no wrap.
			clear(entry.ring[phyStart : phyStart+numEvicted])
		} else {
			// Two segments: tail of ring then beginning.
			clear(entry.ring[phyStart:c])
			clear(entry.ring[0 : numEvicted-(c-phyStart)])
		}
	}
	entry.count = survive
	if entry.count == 0 {
		entry.head = 0
	}
}

// trimAll runs trimJobLocked for every jobID directory under root.
// Called from Scheduler.Start (one cold pass to catch entries that
// went stale during a long process downtime).
func (s *runStore) trimAll(now time.Time) {
	s.trimAllCtx(context.Background(), now)
}

// trimAllCtx is the ctx-aware variant of trimAll. The cold-start GC pass
// can be many ReadDir+Remove syscalls on a large runs/ tree; on a stuck
// FUSE/NFS mount Scheduler.Stop wedges past gcWaitBudget. Passing the
// scheduler stopCtx lets Stop unblock the goroutine between job entries
// (R234-GO-3 / #1019). Inner trimJobLocked is still uninterruptible —
// each job's ReadDir+Remove window is short (≤retention cap) and the
// per-job lock must be held for atomicity, so we only check ctx at job
// boundaries. nil ctx is tolerated for the legacy trimAll() entrypoint.
func (s *runStore) trimAllCtx(ctx context.Context, now time.Time) {
	if s == nil || s.disabled {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			// 非 NotExist 一般指向配置错误（路径指向非目录、权限不足
			// 等），用 Warn 让运维定位；冷启动 GC 失败不致命，记录后继续。
			slog.Warn("cron run: trimAll readdir failed", "root", s.root, "err", err)
		}
		return
	}
	// R20260601-PERF-8 (#1550): collect the job dirs that survive the
	// trim pass so their recentCache warm can fan out across a bounded
	// goroutine pool below. Warming serially in this loop串行化 50job ×
	// keepCount 个 Stat/ReadFile 在启动路径(50×200=10000 次系统调用),
	// 拖慢进程冷启动。warmCacheLocked 对每个 jobID 取独立的 jobLock +
	// entry.mu,跨不同 jobID 并发安全。
	warmJobs := make([]string, 0, len(entries))
	for _, e := range entries {
		// 在每个 job 入口前检查 ctx；scheduler.Stop 触发 stopCancel 后
		// 当前 job 完成即退出循环,避免 Stop 等到 gcWaitBudget 超时。
		if err := ctx.Err(); err != nil {
			slog.Info("cron run: trimAll cancelled mid-pass", "err", err)
			return
		}
		// R234-SEC-10: 跳过 symlink，与 diskListNewestFirst 对 symlink
		// 文件的处理对齐。否则 runs/ 下放置一个指向外部目录的 symlink
		// 目录（IsDir() 为 true 且 jobID 形似有效 hex），trimJobUnderLock
		// 会沿 symlink 进入外部目录做 ReadDir + Remove，构成 path-traversal
		// 写入风险。
		if e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		if !e.IsDir() {
			continue
		}
		jobID := e.Name()
		if !IsValidID(jobID) {
			continue
		}
		// R236-SEC-04 (#489): DT_UNKNOWN bypass on FUSE/tmpfs/NFS surfaces
		// from e.Type() as 0, so a symlink pointing at /etc/cron.d (or any
		// directory the running uid can ReadDir) would slip past the
		// e.Type() ModeSymlink check above and trimJobUnderLock would
		// recurse into it. Defer to e.Info() (a real Lstat) for the
		// authoritative mode and skip anything that isn't a plain
		// directory. This costs one Lstat per top-level entry on the
		// trimAll cold pass — bounded by maxJobsHardCap=500 — vs the
		// defense-in-depth payoff of closing the symlink-bypass window.
		info, infoErr := e.Info()
		if infoErr != nil {
			continue
		}
		if mode := info.Mode(); mode&fs.ModeSymlink != 0 || !mode.IsDir() {
			continue
		}
		s.trimJobUnderLock(jobID, now)
		warmJobs = append(warmJobs, jobID)
	}

	// R250-PERF-9 (#1112) / R20260601-PERF-8 (#1550): pre-warm the
	// recentCache for every surviving job, right after trim settled its
	// on-disk state. Without this, the FIRST dashboard RecentRuns poll after
	// a process restart cold-warms every entry serially on the request path
	// (ReadDir + per-file Lstat + ReadFile + json.Unmarshal up to keepCount
	// per job) — multi-second first-poll latency operators see when the
	// dashboard reconnects. warmCacheLocked is idempotent (skips when
	// entry.warm) so a concurrent Append-driven warm that already fired is a
	// cheap no-op. Each warm takes a per-jobID jobLock + entry.mu, so warming
	// distinct jobIDs concurrently is safe; a bounded pool (mirrors
	// diskDecodeWorkers) collapses the serial 50job×keepCount Stat/ReadFile
	// storm — previously ~10000 serial syscalls on the startup path — into
	// ~ceil(N/diskDecodeWorkers) waves. ctx.Err() short-circuits between
	// dequeues so Stop stays prompt.
	s.warmJobsParallel(ctx, warmJobs)
}

// warmJobsParallel warms the recentCache for each jobID across a bounded
// goroutine pool. Safe because warmCacheLocked acquires a per-jobID jobLock
// and the per-entry mutex internally, so distinct jobIDs never contend. A
// cancelled ctx stops dequeuing new jobs (in-flight warms finish their short
// ReadDir+decode window). R20260601-PERF-8 (#1550).
func (s *runStore) warmJobsParallel(ctx context.Context, jobIDs []string) {
	if len(jobIDs) == 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	workers := diskDecodeWorkers
	if workers > len(jobIDs) {
		workers = len(jobIDs)
	}
	// R20260603-PERF-5: claim work indices via an atomic cursor over the
	// jobIDs slice rather than a per-call make(chan string, len(jobIDs))
	// seeded with N sends + close. Mirrors decodeRunsParallel — on a 50-job
	// cold start the buffered channel was a fresh N-string backing array +
	// channel header; the atomic counter is a single stack int64 and each
	// worker steals the next index with one FetchAdd. Concurrency and the
	// per-jobID warm behaviour are unchanged (warmCacheLocked owns its own
	// locks, so steal order is irrelevant).
	var next int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				i := int(atomic.AddInt64(&next, 1)) - 1
				if i >= len(jobIDs) {
					return
				}
				jobID := jobIDs[i]
				dir := filepath.Join(s.root, jobID)
				warmCorrupt, warmUnreadable := s.warmCacheLocked(jobID)
				if warmCorrupt > 0 {
					slog.Warn("cron runstore: cold-start warm skipped corrupt files",
						"count", warmCorrupt, "dir", dir)
				}
				// R20260603-CR-1 (#1693): separate log for I/O errors.
				if warmUnreadable > 0 {
					slog.Warn("cron runstore: cold-start warm skipped unreadable files",
						"count", warmUnreadable, "dir", dir)
				}
			}
		}()
	}
	wg.Wait()
}

// trimJobUnderLock acquires the per-job lock with defer-unlock so a
// panic inside trimJobLocked (e.g. an FS quirk surfacing through
// os.ReadDir on a FUSE mount) cannot deadlock subsequent Append calls
// for the same jobID.
func (s *runStore) trimJobUnderLock(jobID string, now time.Time) {
	lock := s.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()
	s.trimJobLocked(jobID, now)
}
