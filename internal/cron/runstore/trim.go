package runstore

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// trimJobLocked enforces the per-job retention policy: keep runs satisfying
// BOTH rank ≤ keepCount AND age ≤ keepWindow; either violated → delete. now
// is injected so tests can fix "current time".
//
// Caller must hold jobLock(jobID) on entry AND on exit; the os.Remove batch
// releases it for slow-FS syscall fan-out and re-acquires before the cache
// reconciliation (#712, #1291). No deferred re-acquire is added on panic:
// sync.Mutex is not re-entrant and os.Remove never panics on POSIX.
func (s *Store) trimJobLocked(jobID string, now time.Time) {
	// scanSortedRunDir keeps trim in lockstep with diskListNewestFirst (sort
	// order, symlink filter, ValidID guard) (#871).
	items, _, err := s.scanSortedRunDir(jobID)
	if err != nil {
		return
	}
	cutoff := now.Add(-s.keepWindow)
	// items are newest first: the first entry not after cutoff means every
	// later one is expired too.
	anyExpired := false
	for _, it := range items {
		if !it.mtime.After(cutoff) {
			anyExpired = true
			break
		}
	}
	// Fast path: under cap AND nothing expired → no remove.
	if len(items) <= s.keepCount && !anyExpired {
		return
	}
	if len(items) == 0 {
		return
	}
	// Collect to-remove paths under jobLock, release for the os.Remove batch
	// (FUSE/NFS can take 10s of ms per Remove), re-acquire before
	// cacheTrimAfterDisk (#712). Safe: a concurrent Append during the unlocked
	// window writes a newer-mtime file that is by definition not in toRemove, and
	// cacheTrimAfterDisk counts survivors from the head so it never reaches that row.
	toRemove := make([]string, 0, 4)
	for i, it := range items {
		keep := i < s.keepCount && it.mtime.After(cutoff)
		if keep {
			continue
		}
		toRemove = append(toRemove, it.path)
	}
	if len(toRemove) > 0 {
		// The closure + defer lock.Lock() guarantees the lock is re-acquired even if
		// os.Remove panics; otherwise the caller's deferred Unlock would panic on an
		// unlocked mutex and corrupt lock state for every later Append (#1271).
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
	// Reconcile the cache to the same (count + window) policy; jobLock is held again here.
	s.cacheTrimAfterDisk(jobID, cutoff)
}

// cacheTrimAfterDisk reconciles the recentCache for jobID after on-disk
// trimJobLocked removed expired / over-cap entries. Called by trimJobLocked
// only — caller holds jobLock(jobID).
//
// Trims in place on the ring (head/count + clear of dropped slots);
// ringSnapshot already hands readers fresh copies.
func (s *Store) cacheTrimAfterDisk(jobID string, cutoff time.Time) {
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
	// Ring is newest-first; stop at the first row older than cutoff. The cutoff
	// is mtime-based (trimJobLocked uses ModTime); StartedAt can predate mtime by
	// hours for long runs, so approximate mtime via EndedAt with StartedAt as the
	// fallback for in-progress snapshots. Divergence from disk is < ~1 s
	// pathological and self-heals on the next warm; see cacheStaleEvictionTotal.
	limit := s.keepCount
	if limit > entry.count {
		limit = entry.count
	}
	// ts.Before(cutoff) is monotone over the newest-first ring, so the survive
	// boundary is a sort.Search; changing the cutoff predicate must preserve
	// monotonicity (#930).
	survive := sort.Search(limit, func(i int) bool {
		r := entry.ringRead(i)
		ts := r.EndedAt
		if ts.IsZero() {
			ts = r.StartedAt
		}
		return ts.Before(cutoff)
	})
	// Rows evicted by the approximate time source, so operators can watch the divergence (#962).
	if survive < entry.count {
		s.cacheStaleEvictionTotal.Add(int64(entry.count - survive))
	}
	// Zero the dropped slots to release retained strings: logical slot i lives
	// at ring[(head+i)%c], so the evicted range [survive, count) is one
	// contiguous segment or two if it wraps.
	c := cap(entry.ring)
	if c > 0 && survive < entry.count {
		// Delete evicted RunIDs from the dedup index while entry.count still equals
		// the pre-trim value (ringRead uses logical indices).
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

// TrimAll runs trimJobLocked for every jobID directory under root, then
// pre-warms each surviving job's cache. The scheduler calls it once at start to
// catch entries that went stale during a long downtime. ctx is checked at job
// boundaries so a stuck FUSE/NFS mount cannot wedge Scheduler.Stop past its GC
// budget (#1019); each trimJobLocked stays uninterruptible, since its
// ReadDir+Remove window is short and needs the per-job lock. A nil ctx is
// treated as context.Background().
func (s *Store) TrimAll(ctx context.Context, now time.Time) {
	if s == nil || !s.layout.Enabled() {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	entries, err := os.ReadDir(s.Dir())
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			// 非 NotExist 一般指向配置错误（路径指向非目录、权限不足
			// 等），用 Warn 让运维定位；冷启动 GC 失败不致命，记录后继续。
			slog.Warn("cron run: trimAll readdir failed", "root", s.Dir(), "err", err)
		}
		return
	}
	// Collect surviving job dirs so their recentCache can be warmed below.
	warmJobs := make([]string, 0, len(entries))
	for _, e := range entries {
		// 在每个 job 入口前检查 ctx；scheduler.Stop 触发 stopCancel 后
		// 当前 job 完成即退出循环,避免 Stop 等到 gcWaitBudget 超时。
		if err := ctx.Err(); err != nil {
			slog.Info("cron run: trimAll cancelled mid-pass", "err", err)
			return
		}
		// 跳过 symlink（与 diskListNewestFirst 对齐）：否则指向外部目录的 symlink
		// 会让 trimJobUnderLock 沿 symlink 做 ReadDir + Remove，构成 path-traversal 写入风险。
		if e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		if !e.IsDir() {
			continue
		}
		jobID := e.Name()
		if !ValidID(jobID) {
			continue
		}
		// e.Type() reports 0 (DT_UNKNOWN) on FUSE/tmpfs/NFS, so a symlink can slip
		// past the check above; e.Info() is a real Lstat and is authoritative. One
		// Lstat per top-level entry, bounded by maxJobsHardCap (#489).
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

	// Pre-warm the recentCache for every surviving job so the first dashboard
	// poll after restart does not cold-warm each entry on the request path
	// (#1112). ctx is checked between jobs so Stop is not held up.
	for _, jobID := range warmJobs {
		if ctx.Err() != nil {
			return
		}
		warmCorrupt, warmUnreadable := s.warmCacheLocked(jobID)
		dir := filepath.Join(s.Dir(), jobID)
		if warmCorrupt > 0 {
			slog.Warn("cron runstore: cold-start warm skipped corrupt files",
				"count", warmCorrupt, "dir", dir)
		}
		// Separate log for I/O errors (#1693).
		if warmUnreadable > 0 {
			slog.Warn("cron runstore: cold-start warm skipped unreadable files",
				"count", warmUnreadable, "dir", dir)
		}
	}
}

// trimJobUnderLock acquires the per-job lock with defer-unlock so a
// panic inside trimJobLocked (e.g. an FS quirk surfacing through
// os.ReadDir on a FUSE mount) cannot deadlock subsequent Append calls
// for the same jobID.
func (s *Store) trimJobUnderLock(jobID string, now time.Time) {
	lock := s.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()
	s.trimJobLocked(jobID, now)
}
