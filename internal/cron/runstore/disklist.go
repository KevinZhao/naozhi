package runstore

import (
	"cmp"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// runDirItem is a single .json entry that survived scanSortedRunDir's filter
// pass (regular file, .json suffix, ValidID runID, Info() succeeded). Both
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
func (s *Store) scanSortedRunDir(jobID string) ([]runDirItem, string, error) {
	dir := filepath.Join(s.Dir(), jobID)
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
		if !ValidID(runID) {
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
func (s *Store) diskListNewestFirst(jobID string, limit int, before time.Time) ([]CronRunSummary, int, int) {
	items, dir, err := s.scanSortedRunDir(jobID)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("cron run: list readdir", "dir", dir, "err", err)
		}
		return nil, 0, 0
	}

	out := make([]CronRunSummary, 0, limit)
	corruptCount := 0
	unreadableCount := 0
	for _, it := range items {
		if len(out) >= limit {
			break
		}
		// Strict StartedAt cutoff, no coarse mtime gate (see godoc, #745). readRun's
		// Lstat is skipped: scanSortedRunDir already filtered by ModeSymlink + ValidID.
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
