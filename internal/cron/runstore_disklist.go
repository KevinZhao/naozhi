package cron

import (
	"cmp"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// runDirItem is a single .json entry that survived scanSortedRunDir's filter
// pass (regular file, .json suffix, IsValidID runID, Info() succeeded). Both
// diskListNewestFirst and trimJobLocked consume it, so the ReadDir + filter +
// Stat + sort happens once (#871).
type runDirItem struct {
	path  string // absolute path including .json suffix; safe to os.Remove
	runID string
	mtime time.Time
}

// scanSortedRunDir reads runs/<jobID>/, filters out non-regular / non-hex
// entries, Stat's each survivor, and returns the slice sorted newest first
// (mtime DESC, runID DESC tie-break) plus the dir path. err is fs.ErrNotExist
// when the job has never run; other errors are surfaced verbatim.
//
// Shared by the list and trim paths so both observe the same total order:
// the runID tie-break matters because on low-resolution filesystems two
// records can share an mtime, and the list cutoff (StartedAt < before) and
// trim cutoff (i < keepCount) would otherwise disagree about which one to
// drop. Time.Compare (not UnixNano) keeps wall-clock jumps from desyncing them.
func (s *runStore) scanSortedRunDir(jobID string) ([]runDirItem, string, error) {
	dir := filepath.Join(s.root, jobID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, dir, err
	}
	// Cap at min(len(entries), 2*keepCount): the dir hovers around keepCount
	// plus transient over-cap slack, and non-json orphans (tmp files, dotfiles)
	// are filtered below but would inflate a len(entries) alloc (#940).
	cap0 := len(entries)
	if s.keepCount > 0 {
		bound := 2 * s.keepCount
		if bound < cap0 {
			cap0 = bound
		}
	}
	items := make([]runDirItem, 0, cap0)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// 跳过 symlink，避免有人在 runs/<jobID>/ 目录下放指向 /etc/passwd
		// 之类的软链接诱导 readRun 触发外部文件 read（path traversal 防御）。
		if e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		runID := strings.TrimSuffix(name, ".json")
		if !IsValidID(runID) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// FUSE / some tmpfs / NFS return DT_UNKNOWN, surfaced as e.Type() == 0, so
		// the ModeSymlink check above passes a symlink through. e.Info() just ran a
		// real Lstat, so its Mode is authoritative; re-check here (#489).
		if mode := info.Mode(); mode&fs.ModeSymlink != 0 || !mode.IsRegular() {
			continue
		}
		items = append(items, runDirItem{
			path:  filepath.Join(dir, name),
			runID: runID,
			mtime: info.ModTime(),
		})
	}
	// 0 or 1 items: the sort is a no-op; skip the comparator setup on the
	// common never-/once-run path.
	if len(items) > 1 {
		slices.SortFunc(items, runDirItemNewestFirst)
	}
	return items, dir, nil
}

// runDirItemNewestFirst is scanSortedRunDir's comparator, hoisted to package
// scope so the hot scan path does not allocate a closure per call.
func runDirItemNewestFirst(a, b runDirItem) int {
	// mtime DESC. Time.Compare (not UnixNano) so wall-clock jumps don't desync
	// trim ↔ list ordering.
	if c := b.mtime.Compare(a.mtime); c != 0 {
		return c
	}
	// Equal-mtime tie-break by runID DESC for cross-process stability.
	return cmp.Compare(b.runID, a.runID)
}

// diskListNewestFirst is the on-disk variant of List, used by warmCache and
// as the fall-through when the cache is unavailable or a before-cutoff is set.
// Returns the newest-first summaries plus separate corruptCount (ErrCorruptRun)
// and unreadableCount (EACCES / EIO / ESTALE) so warmCache can log each class
// distinctly (#1693).
//
// There is deliberately NO mtime pre-filter on the before path: mtime ≈ EndedAt
// can exceed `before` while StartedAt is below it (long runs, re-touched files),
// so an mtime gate silently drops paginatable rows; the strict StartedAt filter
// is the only correct one (#745; pinned by TestRunStore_DiskList_BeforeStartedAtMtimeDivergence).
func (s *runStore) diskListNewestFirst(jobID string, limit int, before time.Time) ([]CronRunSummary, int, int) {
	items, dir, err := s.scanSortedRunDir(jobID)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("cron run: list readdir", "dir", dir, "err", err)
		}
		return nil, 0, 0
	}

	// No-cutoff warm path reads every candidate (limit == keepCount, zero cutoff
	// never trims), so the ReadFile+Unmarshal is embarrassingly parallel and the
	// mtime-sorted position index preserves order (#540, #928). Gate on the
	// EFFECTIVE count min(limit, len) so small-limit queries over a large dir stay
	// serial (#929). The before-cutoff path keeps the serial early-break.
	effective := len(items)
	if limit < effective {
		effective = limit
	}
	if before.IsZero() && effective > diskDecodeParallelThreshold {
		return s.decodeRunsParallel(items, limit)
	}

	out := make([]CronRunSummary, 0, limit)
	corruptCount := 0
	unreadableCount := 0
	for _, it := range items {
		if len(out) >= limit {
			break
		}
		// Strict StartedAt cutoff, no coarse mtime gate (see godoc, #745). readRun's
		// Lstat is skipped: scanSortedRunDir already filtered by ModeSymlink + IsValidID.
		run, err := s.readRunNoLstat(it.path)
		if err != nil {
			if errors.Is(err, ErrCorruptRun) {
				corruptCount++
			} else {
				// Non-corrupt read failure (EACCES, EIO, ESTALE): tracked separately (#1693).
				unreadableCount++
			}
			continue
		}
		if !before.IsZero() && !run.StartedAt.Before(before) {
			continue
		}
		out = append(out, run.summary())
	}
	return out, corruptCount, unreadableCount
}

// diskDecodeParallelThreshold is the candidate count above which
// diskListNewestFirst fans the no-cutoff decode out across a worker pool;
// below it the goroutine plumbing costs more than the serial loop saves.
const diskDecodeParallelThreshold = 16

// diskDecodeWorkers caps the concurrent ReadFile+Unmarshal fan-out used by
// decodeRunsParallel so a cold-start prewarm storm cannot open N × keepCount
// file descriptors at once.
const diskDecodeWorkers = 8

// decodeSlot is the position-indexed scratch entry decodeRunsParallel writes
// each decode into so completion order is irrelevant (items is mtime-sorted).
// Package scope so the backing array can be recycled via decodeSlotPool (#1924).
type decodeSlot struct {
	summary CronRunSummary
	ok      bool
	corrupt bool
}

// decodeSlotPool recycles the []decodeSlot scratch slice across
// decodeRunsParallel calls (a 50-job cold start otherwise allocates 50 × n
// slots). The slice is resliced and cleared before reuse so a stale ok=true
// from a prior call can never leak into the result (#1924).
var decodeSlotPool = sync.Pool{
	New: func() any {
		s := make([]decodeSlot, 0, DefaultRunsKeepCount)
		return &s
	},
}

// decodeSlotPoolMaxCap drops oversized backing arrays from the pool so an
// operator who raised RunsKeepCount well above the default for one job does
// not pin an outsized array for the process lifetime. Sized at 2× the default
// cap for headroom around the common case.
const decodeSlotPoolMaxCap = 2 * DefaultRunsKeepCount

// decodeRunsParallel reads + decodes the supplied newest-first items across
// a bounded worker pool and returns the summaries in the SAME newest-first
// order plus separate corrupt / unreadable counts (#1693). Order is preserved
// by writing each decode into a position-indexed scratch slice. Only called
// from the before.IsZero path. Valid rows are collected newest-first until
// `limit` are gathered (#2150), so the counts cover exactly the scanned prefix,
// identical to the serial loop's early-break accounting.
func (s *runStore) decodeRunsParallel(items []runDirItem, limit int) ([]CronRunSummary, int, int) {
	// Decode the full candidate window (≤ 2*keepCount in the over-cap window),
	// not min(len, limit): the serial path backfills past corrupt/unreadable files
	// from older candidates until it has `limit` valid rows, and hard-trimming to
	// `limit` here would return fewer rows than the serial path (#2150).
	n := len(items)
	// Reuse the pooled scratch slice when its cap suffices; clear so no stale
	// entry survives (#1924).
	slotsPtr := decodeSlotPool.Get().(*[]decodeSlot)
	defer func() {
		if cap(*slotsPtr) > decodeSlotPoolMaxCap {
			return
		}
		decodeSlotPool.Put(slotsPtr)
	}()
	if cap(*slotsPtr) >= n {
		*slotsPtr = (*slotsPtr)[:n]
		clear(*slotsPtr)
	} else {
		*slotsPtr = make([]decodeSlot, n)
	}
	slots := *slotsPtr
	workers := diskDecodeWorkers
	if workers > n {
		workers = n
	}
	// Atomic cursor instead of a buffered channel: one FetchAdd per steal, no
	// per-call alloc; order comes from the position index.
	var next int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for {
				i := int(atomic.AddInt64(&next, 1)) - 1
				if i >= n {
					return
				}
				run, err := s.readRunNoLstat(items[i].path)
				if err != nil {
					if errors.Is(err, ErrCorruptRun) {
						slots[i].corrupt = true
					}
					// !corrupt case: slots[i].ok stays false; counted below.
					continue
				}
				slots[i].summary = run.summary()
				slots[i].ok = true
			}
		}()
	}
	wg.Wait()

	// Mirror the serial accumulate-until-`limit` walk: backfill past
	// corrupt/unreadable slots and STOP once `limit` valid rows are gathered.
	out := make([]CronRunSummary, 0, limit)
	corruptCount := 0
	unreadableCount := 0
	for i := range slots {
		if len(out) >= limit {
			break
		}
		if slots[i].ok {
			out = append(out, slots[i].summary)
			continue
		}
		if slots[i].corrupt {
			corruptCount++
		} else {
			// Non-corrupt read failure: tracked separately from corrupt (#1693).
			unreadableCount++
		}
	}
	return out, corruptCount, unreadableCount
}
