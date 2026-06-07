package cron

// Disk-listing / scan / parallel-decode cluster for runStore. Moved verbatim
// from runstore.go (move-only, #1282); behaviour and byte-for-byte bodies are
// unchanged. The cross-file callers (trimJobLocked → scanSortedRunDir,
// warmJobsParallel → diskDecodeWorkers) stay in runstore.go; same-package
// references resolve as before.

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

// runDirItem is a single .json entry that survived the filter pass in
// scanSortedRunDir: regular file (not a dir / not a symlink), .json
// suffix, IsValidID runID, Info() succeeded. Both diskListNewestFirst
// (warmCache path) and trimJobLocked (Append GC path) consume the same
// shape, so caching it once eliminates the duplicate ReadDir + filter +
// Stat + sort that R239-PERF-5 (#871) flagged.
type runDirItem struct {
	path  string // absolute path including .json suffix; safe to os.Remove
	runID string
	mtime time.Time
}

// scanSortedRunDir reads runs/<jobID>/, filters out non-regular / non-hex
// entries, Stat's each survivor, and returns the slice sorted newest
// first (mtime DESC, runID DESC tie-break). The dir path is returned
// alongside so callers can build paths or log without re-running
// filepath.Join. err is fs.ErrNotExist when the job has never run; other
// errors are surfaced verbatim so callers can decide whether to slog.
//
// R239-PERF-5 (#871): originally diskListNewestFirst and trimJobLocked
// each open-coded the same scan loop + sort. On Append's hot path the
// cold-cache scenario flowed (cacheGet → warmCache → diskListNewestFirst)
// then later (Append → trimJobLocked), running ReadDir + Stat-per-entry
// twice for the same directory. Sharing the scan keeps both paths in
// lockstep (any sort-order or filter-policy change lands once) and
// halves the ReadDir + per-entry Stat work — important on FUSE / NFS
// where each e.Info() Stat is a separate round-trip.
//
// Sort policy:
//   - mtime DESC: newer first; mtime ≈ WriteFileAtomic landing time, the
//     coarse total order callers want. R235-PERF-17 keeps Time.Compare
//     instead of UnixNano so wall-clock jumps / monotonic resets do not
//     desync the ordering between trim and list paths (R236-QA-01).
//   - runID DESC tie-break: when low-resolution filesystems collapse two
//     concurrent atomic writes to the same nanosecond, ReadDir iteration
//     order becomes load-bearing without a deterministic key — the
//     pagination cutoff in diskListNewestFirst (StartedAt < before) and
//     the cap cutoff in trimJobLocked (i < keepCount) would otherwise
//     disagree about which equal-mtime record to drop, leaving a record
//     visible in the list that trim deletes (or vice versa). runID is
//     16-char random hex, so the tie-break is stable across processes
//     and re-reads. R222-GO-5 / R235-GO-7.
func (s *runStore) scanSortedRunDir(jobID string) ([]runDirItem, string, error) {
	dir := filepath.Join(s.root, jobID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, dir, err
	}
	// R249-PERF-25 (#940): cap is bounded by min(len(entries), 2*keepCount).
	// trimJobLocked keeps runs/<jobID>/ around keepCount entries (default
	// 200) plus transient slack for in-progress writes. Sizing the slice
	// to len(entries) over-allocates whenever the directory accumulates
	// non-json orphans (atomic-write tmp files crashed mid-rename, hidden
	// dotfiles, .DS_Store, operator scratch) — those entries are filtered
	// in the loop below but still pay the initial alloc. The 2× factor
	// gives headroom for a brief over-cap window between finishRun's
	// Append and the subsequent trimJobLocked, while keepCount=0 (a
	// disabled / mis-configured store) falls back to len(entries) so we
	// don't degrade to a many-realloc growth curve. min(...) also handles
	// the tiny-dir case where len(entries) < 2*keepCount and the cap
	// equals the historical value — no regression for the warm steady
	// state.
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
		// R236-SEC-04 (#489): some filesystems (FUSE, certain tmpfs / NFS
		// configurations) return DT_UNKNOWN from getdents, which Go surfaces
		// as e.Type() == 0. The earlier ModeSymlink check on e.Type() then
		// silently passes a symlink through; trim's downstream os.Remove and
		// list's readRunNoLstat would follow it. e.Info() above just ran
		// Lstat (the documented behaviour for DirEntry.Info on a Type with
		// no cached d_type), so the resulting FileInfo's Mode is the
		// authoritative kind. Re-check ModeSymlink + IsRegular here to close
		// the DT_UNKNOWN bypass; the cost is zero on filesystems that fill
		// d_type (the e.Type() check already handled them) and amounts to
		// one extra Mode-bit comparison on the slow-FS path that already
		// paid an Lstat above.
		if mode := info.Mode(); mode&fs.ModeSymlink != 0 || !mode.IsRegular() {
			continue
		}
		items = append(items, runDirItem{
			path:  filepath.Join(dir, name),
			runID: runID,
			mtime: info.ModTime(),
		})
	}
	// R260528-PERF-25 (#1361): a job that has only ever run once (or whose
	// dir is empty after a trim) has 0 or 1 surviving items, for which the
	// sort is a guaranteed no-op. Skip slices.SortFunc on that case so the
	// cold cacheGet → warmCache → scanSortedRunDir path for never-/once-run
	// jobs (common right after a fresh job is created) avoids the comparator
	// setup. >1 items still take the full mtime-DESC + runID tie-break sort
	// so the cross-process ordering contract (see godoc) is unchanged.
	if len(items) > 1 {
		slices.SortFunc(items, runDirItemNewestFirst)
	}
	return items, dir, nil
}

// runDirItemNewestFirst is the shared comparator for scanSortedRunDir's
// mtime-DESC ordering. Hoisted out of the call site to a package-level
// function so it is NOT reallocated as a fresh closure header on every
// scan: scanSortedRunDir runs on both the trim (Append GC) path and the
// cold-cache list/warm path, so at process restart with N jobs it fires
// 2×N times, each previously allocating a no-capture closure. Mirrors the
// R20260527122801-PERF-2 (#1340) fix that lifted jobIDCmpForSort out of
// marshalJobsLocked for the same reason — behaviour is identical.
func runDirItemNewestFirst(a, b runDirItem) int {
	// mtime DESC: newer first. Time.Compare (Go 1.20+) instead of
	// UnixNano so wall-clock jumps don't desync trim ↔ list ordering
	// (R236-QA-01). R235-PERF-17.
	if c := b.mtime.Compare(a.mtime); c != 0 {
		return c
	}
	// Equal-mtime tie-break by runID DESC for cross-process stability.
	// R222-GO-5.
	return cmp.Compare(b.runID, a.runID)
}

// diskListNewestFirst is the on-disk variant of List, used by warmCache
// and as the fall-through when cache is unavailable / before-cutoff is
// set. Returns the summary list and the count of corrupt/unreadable run
// files skipped during the scan (R20260526-CR-018, #1227 — surfaces
// silent skips so warmCache can emit a single aggregate log line).
//
// R239-PERF-5 (#871): scan + sort delegated to scanSortedRunDir so this
// path stays in lockstep with trimJobLocked.
//
// PROPOSAL HISTORY (won't-fix as proposed):
//   - R237-PERF-8 / #682 ("no mtime pre-filter — full JSON parse before
//     discard") and R236-PERF-07 / #522 ("binary-search for `before`
//     cutoff; ReadFile only items within the requested page") both
//     proposed an mtime gate as a fast path. Both rejected because
//     mtime ≥ before does NOT imply StartedAt ≥ before — long-running
//     jobs that started before the cutoff but ended after it (or
//     re-touched their file via process restart) would be silently
//     dropped from the page. R246-CR-008 / #745 already removed the
//     unsafe gate after operators reported phantom "no older runs"
//     truncation; the strict StartedAt filter is the only correct one
//     and the per-candidate ReadFile is the cost of correctness here.
//   - R260528-PERF-20 / #1359 ("mtime ceiling gate when mtime < before")
//     is the symmetric variant: in steady-state cron jobs (run-time
//     much smaller than the pagination window) StartedAt ≈ EndedAt ≈
//     mtime, so an mtime < before pre-filter would skip the ReadFile
//     for files that the strict StartedAt filter would also drop. Same
//     won't-fix as #682 / #522: the asymmetry is one-directional —
//     mtime > StartedAt is possible (long runs, fsnotify-touched files,
//     filesystem mtime drift on restart), so a coarse mtime gate would
//     still suppress legitimately-paginatable rows. Maintaining a
//     metadata index sidecar adds a second consistency surface that
//     warmCache + trimJobLocked + gc would all need to honour; the
//     correctness cost outweighs the per-page IO saving on a path that
//     already serves cache-warm pages without disk IO via cacheGet.
//   - The regression scenario is locked in by
//     TestRunStore_DiskList_BeforeStartedAtMtimeDivergence in
//     runstore_test.go; any future re-introduction of an mtime gate
//     must keep that test green.
//
// diskListNewestFirst returns the newest-first summaries plus two separate
// failure counts: corruptCount (ErrCorruptRun files) and unreadableCount
// (other I/O failures such as EACCES / EIO / ESTALE). Keeping them separate
// lets warmCache log a distinct message for each class so operators can
// distinguish data corruption from transient filesystem errors.
// R20260603-CR-1 (#1693).
func (s *runStore) diskListNewestFirst(jobID string, limit int, before time.Time) ([]CronRunSummary, int, int) {
	items, dir, err := s.scanSortedRunDir(jobID)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("cron run: list readdir", "dir", dir, "err", err)
		}
		return nil, 0, 0
	}

	// R247-PERF-9 (#540) / R249-PERF-7 (#928): the no-cutoff warm path
	// (before.IsZero) reads up to keepCount candidates — on a cold cache
	// that is keepCount × ReadFile + UnmarshalJSON serialised under
	// jobLock. Since every surviving candidate is read in this regime
	// (no early break: limit == keepCount when warmCache drives the call,
	// and the StartedAt filter never trims a zero cutoff), the decode is
	// embarrassingly parallel: fan the ReadFile+Unmarshal out across a
	// bounded worker pool and reassemble in newest-first order. The
	// per-job mtime sort already fixed the order, so a position-indexed
	// result slice preserves it regardless of completion order. The
	// before-cutoff (pagination) path keeps the serial early-break: it
	// stops at the first `limit` matches and parallelising would over-read
	// past the page boundary.
	//
	// R249-PERF-8 (#929): gate on the EFFECTIVE read count min(limit, len)
	// rather than len(items) alone. decodeRunsParallel only decodes the
	// newest min(limit, len) candidates, so a query with a small limit over
	// a large directory (e.g. limit=5 against a 200-run dir) would otherwise
	// spin up the worker pool + channel plumbing to read just `limit` files —
	// the exact "decide on full dir size, then trim to limit" smell. Keeping
	// such small-limit reads on the serial early-break path avoids the
	// goroutine churn; the warm cold-start path (limit == keepCount) still
	// crosses the threshold and parallelises as before. Output is identical
	// either way (both honour newest-first mtime order).
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
		// R246-CR-008 (#745): pagination key consistency. The strict cutoff
		// below filters on StartedAt; an earlier coarse mtime gate
		// (`!it.mtime.Before(before)` skip) was unsafe in one direction —
		// a long-running job with StartedAt < before but mtime (≈ EndedAt)
		// ≥ before would be skipped here without ever reading the file,
		// even though it should appear in the page. The previous comment
		// dismissed the asymmetry as "rare in practice", but operators
		// configuring ExecTimeout to span the pagination window (or any
		// process restart that bumps mtime via re-touch) hits it
		// silently — page truncation looked like "no older runs" instead
		// of pagination skip. Drop the coarse gate; readRunNoLstat is
		// cheap enough on the typical limit≤50 page that one ReadFile per
		// candidate is acceptable for correctness here. Items are still
		// sorted newest-first by mtime so the StartedAt strict filter
		// applies to the correct prefix of candidates.
		//
		// R245-PERF-9: skip readRun's Lstat — scanSortedRunDir already
		// filtered each DirEntry by ModeSymlink + IsValidID, so the
		// IsRegular() recheck would just duplicate the syscall.
		run, err := s.readRunNoLstat(it.path)
		if err != nil {
			if errors.Is(err, ErrCorruptRun) {
				corruptCount++
			} else {
				// Non-corrupt read failure (e.g. EACCES, EIO, ESTALE):
				// track separately so warmCache can log a distinct message.
				// R20260603-CR-1 (#1693).
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
// diskListNewestFirst fans the no-cutoff decode out across a worker pool.
// Below it the goroutine + channel plumbing costs more than the serial
// ReadFile loop saves, so the warm-but-small (≤ this many runs) job stays
// on the serial path. Sized so a typical handful-of-runs job never spins
// up workers.
const diskDecodeParallelThreshold = 16

// diskDecodeWorkers caps the concurrent ReadFile+Unmarshal fan-out used by
// decodeRunsParallel. Bounded so a 50-job cold-start prewarm storm cannot
// open 50 × keepCount file descriptors at once — each job's warm holds its
// jobLock, so the bound is per-job and the global fd ceiling is
// maxConcurrentWarm × diskDecodeWorkers, not N × keepCount.
const diskDecodeWorkers = 8

// decodeSlot is the position-indexed scratch entry decodeRunsParallel writes
// each parallel decode into so completion order is irrelevant (items is
// already mtime-sorted). Hoisted to package scope (was a function-local type)
// so the []decodeSlot backing array can be recycled via decodeSlotPool
// instead of freshly allocated on every cold-cache warm. R20260607-PERF-012
// (#1924).
type decodeSlot struct {
	summary CronRunSummary
	ok      bool
	corrupt bool
}

// decodeSlotPool recycles the []decodeSlot scratch slice across
// decodeRunsParallel calls. Every cold-cache warm previously allocated a fresh
// make([]decodeSlot, n) (n up to keepCount=200); a 50-job cold start
// (trimAllCtx-then-warm) was 50 × that allocation. Pooling drops it to
// amortised zero after warmup. The slice is reset to its full capacity and
// cleared before reuse so a stale ok=true / summary from a prior (larger) call
// can never leak into the result. R20260607-PERF-012 (#1924).
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
// order plus separate counts of corrupt files (ErrCorruptRun) and unreadable
// files (other I/O errors such as EACCES / EIO / ESTALE). Separating the two
// lets warmCache emit distinct log messages so operators can distinguish data
// corruption from transient filesystem errors. R20260603-CR-1 (#1693).
//
// Order is preserved by writing each decode into a position-indexed scratch
// slice (items is already mtime-sorted by scanSortedRunDir), so completion
// order is irrelevant. Only called from the before.IsZero path where every
// candidate up to limit is wanted, so there is no early-break to honour.
func (s *runStore) decodeRunsParallel(items []runDirItem, limit int) ([]CronRunSummary, int, int) {
	n := len(items)
	if n > limit {
		// before.IsZero means no StartedAt filter drops rows, so the first
		// `limit` (newest) candidates are exactly the answer — decode only
		// those rather than the whole directory.
		n = limit
	}
	// R20260607-PERF-012 (#1924): recycle the position-indexed scratch slice
	// via decodeSlotPool rather than make([]decodeSlot, n) per warm. Grow to n
	// (reusing the pooled backing array when its cap suffices) and clear so no
	// stale entry from a prior call survives into this result.
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
	// R20260602190132-PERF-9: claim work indices via an atomic cursor rather
	// than a per-call make(chan int, n) seeded with n sends + close. On a
	// 50-job cold start (n up to keepCount=200) the buffered channel was a
	// fresh ~200-int backing array + channel header per job; the atomic
	// counter is a single stack int64 and each worker steals the next index
	// with one CAS-free FetchAdd. Order is still preserved by the
	// position-indexed slots slice, so steal order is irrelevant.
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

	out := make([]CronRunSummary, 0, n)
	corruptCount := 0
	unreadableCount := 0
	for i := range slots {
		if slots[i].corrupt {
			corruptCount++
		} else if !slots[i].ok {
			// Non-corrupt read failure (e.g. EACCES, EIO, ESTALE): track
			// separately from corrupt so warmCache can log a distinct message.
			// R20260603-CR-1 (#1693).
			unreadableCount++
		}
		if slots[i].ok {
			out = append(out, slots[i].summary)
		}
	}
	return out, corruptCount, unreadableCount
}
