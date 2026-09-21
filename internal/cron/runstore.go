package cron

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/runlog"
)

// runStore persists CronRun records under runsRoot (the cron store's own
// datadir.Layout.RunsRoot(), NEVER the session store's):
//
//	runs/<jobID>/<run_id>.json    # one record per run; ~2KB typical
//
// Per-job file ops are serialised by a sync.Map of *sync.Mutex keyed on jobID;
// WriteFileAtomic relies on rename uniqueness. Scheduler.storeMu is NOT shared.
//
// Lock hierarchy: Scheduler.s.mu > runStore.jobLock(jobID) > recentCacheEntry.mu.
// 已持 entry.mu 时禁止再获取 jobLock 或 s.tbl.mu（cacheGet 走"释放-重取"模式）。
// Errors are surfaced via slog, never returned: cron must not block on history failure.
type runStore struct {
	// layout owns the runs/ tree: root validation, the per-job directory, the
	// atomic record write, the per-job lock and the write-failure counters,
	// shared with session/runhistory (internal/runlog, #2709). A disabled
	// layout is how StorePath="" spells "no persistence".
	layout *runlog.Layout
	// keepCount / keepWindow / maxRunBytes 在 newRunStore 之后不可变，读取无需锁。
	keepCount   int
	keepWindow  time.Duration
	maxRunBytes int64
	// clock is the time source Append reads. nil = time.Now(), so production and
	// every existing construction path are unchanged. It exists so a test can
	// prove Append reads the clock ONCE and shares that instant between
	// skipAppendTrim and trimJobLocked (R20260603-PERF-11) — with a stepping
	// clock, two reads produce two different cutoffs and the trim decision
	// diverges observably. Before this, that property was pinned by a regexp over
	// this file (Epic I #2547).
	clock        cronClock
	enableTrimGC bool // true in production; tests can disable for determinism

	// recentCache memoises the newest-N summaries per job so the dashboard list
	// poll does not hit disk. Populated by Append, trimmed by trimJobLocked.
	// Each entry is owned by the same jobLock as the on-disk subtree; callers
	// always receive a fresh copy, never the slice itself.
	recentCache sync.Map // jobID -> *recentCacheEntry

	// cacheGetPostWarmHook is a test-only seam invoked by cacheGet between
	// warmCache and the post-warm re-Load. Always nil in production (#2000).
	cacheGetPostWarmHook func(jobID string)

	// appendPreWriteHook is a test-only seam invoked by Append after marshal and
	// before ensureJobDir + WriteFileAtomic, so tests can land a DeleteJob inside
	// the window that the post-write re-check + dropOrphanRun closes (#2479).
	// Always nil in production.
	appendPreWriteHook func(jobID string)

	// historyDropTotal counts CronRun records Append dropped because even the
	// truncated retry payload exceeded maxRunBytes; reconciles
	// CronRunStartedTotal − CronRunEndedTotal against missing history rows (#964).
	historyDropTotal atomic.Int64

	// cacheStaleEvictionTotal counts recentCache rows cacheTrimAfterDisk evicted
	// using its EndedAt/StartedAt approximation rather than disk mtime. A growing
	// delta vs disk-side trims means the approximation is evicting rows whose
	// files are still kept (#962).
	cacheStaleEvictionTotal atomic.Int64
}

// HistoryDropTotal returns the cumulative count of CronRun records Append
// dropped because the truncated retry payload still exceeded maxRunBytes.
// Monotonically non-decreasing; returns 0 when s is nil or disabled.
func (s *runStore) HistoryDropTotal() int64 {
	if s == nil {
		return 0
	}
	return s.historyDropTotal.Load()
}

// CacheStaleEvictionTotal returns the cumulative count of recentCache rows
// evicted by cacheTrimAfterDisk's approximate time source. Monotonically
// non-decreasing; returns 0 when s is nil or disabled.
func (s *runStore) CacheStaleEvictionTotal() int64 {
	if s == nil {
		return 0
	}
	return s.cacheStaleEvictionTotal.Load()
}

// WriteFailedTotals returns the cumulative count of CronRun record-write
// failures, split so operators can distinguish ENOSPC from EACCES / IO errors.
// Append cannot return errors, so this counter plus the Error log is the only
// signal a lost record leaves behind (#1338). Owned by the shared layout.
func (s *runStore) WriteFailedTotals() (diskFull, other int64) {
	if s == nil {
		return 0, 0
	}
	return s.layout.WriteFailedTotals()
}

// enabled reports whether this runStore will persist / serve run history,
// folding the nil receiver and the disabled flag (StorePath empty) into one
// predicate so callers do not hand-roll `s.runStore != nil` (#993).
func (s *runStore) enabled() bool {
	return s != nil && s.layout.Enabled()
}

// rootDir is the validated runs/ root, or "" when the store is disabled. The
// layout is the single source of it; this accessor keeps the path-composing
// call sites (disk list, trim, readRun) reading one field.
func (s *runStore) rootDir() string {
	if s == nil {
		return ""
	}
	return s.layout.Root()
}

// Defaults (DefaultRunsKeepCount / DefaultRunsKeepWindow) and hard caps
// (MaxRunRecordBytes) live in limits.go.

// ErrCorruptRun is returned when a run JSON file fails to parse or
// exceeds the size cap. Treated identically to "missing": list APIs
// skip the entry, GC removes it.
var ErrCorruptRun = errors.New("cron run: corrupt or oversize record")

// newRunStore constructs a runStore rooted at <storePath dir>/runs.
// storePath="" disables the store (List returns empty, Append no-ops).
//
// The root is normalised via filepath.Abs + Clean so `..` segments in
// storePath cannot escape the data dir, and a runs/ that is a symlink or
// non-directory disables the store: a symlinked runs/ would redirect every
// CronRun write to an arbitrary tree (#825).
//
// maxBytesOpt overrides the MaxRunRecordBytes per-record cap; non-positive
// or absent falls back to the default (#512).
func newRunStore(storePath string, keepCount int, keepWindow time.Duration, maxBytesOpt ...int64) *runStore {
	if storePath == "" {
		return &runStore{layout: runlog.New(runlog.Options{})}
	}
	if keepCount <= 0 {
		keepCount = DefaultRunsKeepCount
	}
	if keepWindow <= 0 {
		keepWindow = DefaultRunsKeepWindow
	}
	maxBytes := int64(MaxRunRecordBytes)
	if len(maxBytesOpt) > 0 && maxBytesOpt[0] > 0 {
		maxBytes = maxBytesOpt[0]
	}
	// datadir names the tree; runlog validates it (Abs + Clean, refuse a
	// symlinked or non-directory root, tighten a loose mode) and owns every
	// guard that used to live inline here.
	layout := runlog.New(runlog.Options{
		Root:  datadir.ForStore(storePath).RunsRoot(),
		Label: "cron run",
	})
	return &runStore{
		layout:       layout,
		keepCount:    keepCount,
		keepWindow:   keepWindow,
		maxRunBytes:  maxBytes,
		enableTrimGC: true,
	}
}

// jobLock returns a *sync.Mutex unique to jobID. Lazily allocated and
// reclaimed by DeleteJob so the live set tracks the live job set (#971); a
// deleted job racing a concurrent Append on the same ID is the same edge
// handled by the runningJobs sync.Map.
func (s *runStore) jobLock(jobID string) *sync.Mutex {
	return s.layout.Lock(jobID)
}

// assertJobLockHeld logs a warning when jobLock(jobID) is currently free —
// the signature of a caller that violated the *Locked-suffix contract.
// Best-effort: it warns rather than panics because cron history must never
// take the scheduler down (#696, #694), false negatives under contention are
// accepted, and the TryLock probe only runs under `go test` since it sits on
// the Append hot path (#961).
func (s *runStore) assertJobLockHeld(jobID string) {
	s.layout.AssertLockHeld(jobID)
}

// Append writes one run record to disk and trims the per-job ring.
// Errors are logged, never returned: cron must not block history failure.
func (s *runStore) Append(run *CronRun) {
	if !s.enabled() || run == nil || run.JobID == "" || run.RunID == "" {
		return
	}
	if !IsValidID(run.RunID) {
		slog.Warn("cron run: skipping append with invalid run_id", "run_id", run.RunID)
		return
	}
	if !IsValidID(run.JobID) {
		// 非 hex 的 jobID 可能是测试 fixture / 篡改文件：拒绝 append 而非创建可疑目录。
		slog.Warn("cron run: skipping append with non-hex job_id", "job_id", run.JobID)
		return
	}

	// Marshal + over-cap shrink are pure CPU on the caller-owned *run, so they
	// run outside jobLock (#549).
	// Preflight: when Result+Prompt+ErrorMsg alone overshoot the cap minus a fixed
	// headroom, skip the doomed first marshal; the post-marshal gate stays authoritative (#1111).
	const fixedFieldsHeadroom = 1024
	preflightOverCap := s.maxRunBytes > fixedFieldsHeadroom &&
		int64(len(run.Result)+len(run.Prompt)+len(run.ErrorMsg)) >
			s.maxRunBytes-fixedFieldsHeadroom
	var err error
	// rec starts as the untruncated record paired with its own summary; the
	// over-cap branches replace it wholesale. Payload and summary only travel
	// together inside one runRecord, and both over-cap branches go through the
	// same applyShrink, so the specific mistake #1079 fixed — a branch that
	// updates the payload and forgets the cache source — has nowhere left to
	// happen. Producing the summary from the wrong record inside
	// shrinkAndMarshal is still writable; that one line is the whole surface.
	rec := runRecord{summary: run.summary()}
	// applyShrink returns false when even the truncated record does not fit, in
	// which case the run is dropped and the caller must return.
	applyShrink := func() bool {
		shrunk, fits, err2 := s.shrinkAndMarshal(run)
		if !fits {
			// err2 may be nil when the truncated payload still exceeds
			// maxRunBytes (metadata alone over cap), so log both err2 and the
			// post-truncate size.
			retryBytes := -1
			if err2 == nil {
				retryBytes = len(shrunk.payload)
			}
			s.historyDropTotal.Add(1)
			slog.Warn("cron run: retry marshal also exceeded cap; run record dropped",
				"job_id", run.JobID,
				"run_id", run.RunID,
				"retry_err", err2,
				"retry_bytes", retryBytes,
				"cap", s.maxRunBytes)
			return false
		}
		rec = shrunk
		return true
	}
	if preflightOverCap {
		// Distinct message so preflight over-cap is distinguishable from the post-marshal retry.
		slog.Warn("cron run: preflight over-cap: truncating result/prompt directly (skipping full marshal)",
			"job_id", run.JobID, "run_id", run.RunID,
			"preflight_bytes", len(run.Result)+len(run.Prompt)+len(run.ErrorMsg),
			"cap", s.maxRunBytes)
		if !applyShrink() {
			return
		}
	} else {
		rec.payload, err = marshalRunPooled(run)
		if err != nil {
			slog.Warn("cron run: marshal failed", "job_id", run.JobID, "run_id", run.RunID, "err", err)
			return
		}
	}
	if int64(len(rec.payload)) > s.maxRunBytes {
		slog.Warn("cron run: payload exceeds size cap; truncating result/prompt and retrying",
			"job_id", run.JobID, "run_id", run.RunID, "bytes", len(rec.payload), "cap", s.maxRunBytes)
		// 退化路径：把 Result 砍到极短，重新 marshal。Prompt 亦同。
		// 这里不返回 — 一定要落盘一条记录，UI 才能看到 "曾有这么一条 run"。
		if !applyShrink() {
			return
		}
	}
	// The disk write runs OUTSIDE jobLock: each Append writes a unique
	// <runID>.json (rename-atomic), so concurrent Appends do not collide, and
	// holding the lock across fsync+rename serialised every Append behind a slow
	// disk (#1335). The warmCache-reads-new-file-before-cacheHeadPush interleave
	// is neutralised by the RunID dedup inside cacheHeadPush.
	if s.appendPreWriteHook != nil {
		s.appendPreWriteHook(run.JobID)
	}
	// WriteRecord does the containment check (#484), re-validates the per-job
	// directory against a symlink swap (#1968), fsyncs the root for a fresh
	// subdir (#976) and counts the failure split by ENOSPC (#1338) — all of
	// which used to live inline here and now live once, in internal/runlog.
	if err := s.layout.WriteRecord(run.JobID, run.RunID, rec.payload); err != nil {
		// Append cannot return an error (history is best-effort), so this log
		// plus the layout's counter is all a lost record leaves behind.
		slog.Error("cron run: write failed; run record dropped",
			"err", err, "disk_full", osutil.IsDiskFull(err),
			"job_id", run.JobID, "run_id", run.RunID)
		return
	}

	// Cache push + trim run under jobLock so cacheHeadPush / cacheGetBefore /
	// trimJobLocked stay serialised per job; the critical section is now
	// O(µs) ring updates, not O(fsync). cacheHeadPush no-ops on a cold cache.
	lock := s.jobLock(run.JobID)
	lock.Lock()
	defer lock.Unlock()
	s.cacheHeadPush(run.JobID, rec.summary)
	if s.enableTrimGC {
		// One clock read shared by skipAppendTrim and trimJobLocked.
		now := s.now()
		if !s.skipAppendTrim(run.JobID, now) {
			s.trimJobLocked(run.JobID, now)
		}
	}
}

// maxRetryFieldRunes 是 over-cap retry 路径每个字段（Result/Prompt/ErrorMsg）
// 的最大 rune 数。三处共用同一上限保证退化记录字节数可估算，不易再触发 maxRunBytes。
const maxRetryFieldRunes = 256

// List returns up to limit summaries for jobID, newest first. before is
// a unix-ms cutoff: only runs with StartedAt < before are returned (paging).
// Zero before = no cutoff. Errors during read are logged and the entry
// skipped; callers always receive a (possibly partial) list.
func (s *runStore) List(jobID string, limit int, before time.Time) []CronRunSummary {
	if !s.enabled() || jobID == "" {
		return nil
	}
	if !IsValidID(jobID) {
		return nil
	}
	if limit <= 0 {
		limit = 50
	}
	// Clamp to the configured retention cap, not the package default, so an
	// operator-raised RunsKeepCount can be paged (#969).
	if limit > s.keepCount {
		limit = s.keepCount
	}

	// Cache fast-path: zero before + warm entry returns without IO.
	if before.IsZero() {
		if cached, ok := s.cacheGet(jobID, limit); ok {
			return cached
		}
	} else {
		// While the cache has not hit keepCount the ring holds every on-disk row,
		// so filtering the cache equals a disk scan; once full, trim may have shed
		// rows the caller would miss, so fall through to disk (#810).
		if cached, ok := s.cacheGetBefore(jobID, limit, before); ok {
			return cached
		}
	}
	rows, corruptCount, unreadableCount := s.diskListNewestFirst(jobID, limit, before)
	if corruptCount > 0 {
		slog.Warn("cron runstore List skipped corrupt run files", "count", corruptCount, "job_id", jobID)
	}
	if unreadableCount > 0 {
		slog.Warn("cron runstore List skipped unreadable run files", "count", unreadableCount, "job_id", jobID)
	}
	return rows
}

// Recent returns the N most recent CronRunSummary entries for jobID
// (newest first): List with limit=n, before=zero.
func (s *runStore) Recent(jobID string, n int) []CronRunSummary {
	return s.List(jobID, n, time.Time{})
}

// RecentSessionIDs returns up to n distinct non-empty SessionID strings from
// the newest-first run history for jobID. Equivalent to reading SessionID off
// Recent(jobID, n) but skips the per-row CronRunSummary copy (Result up to
// ~4 KB) on the buildKnownSessionsSet hot path (#1285). Cache-warm path is
// O(min(n, count)) under entry.mu; cold path falls back to List+filter.
//
// Returns a fresh slice; empty when the job never ran or no run has a
// SessionID. Limit clamping mirrors List.
func (s *runStore) RecentSessionIDs(jobID string, n int) []string {
	if !s.enabled() || jobID == "" {
		return nil
	}
	if !IsValidID(jobID) {
		return nil
	}
	if n <= 0 {
		n = 50
	}
	// Clamp to the configured retention cap, mirroring List (#969).
	if n > s.keepCount {
		n = s.keepCount
	}
	// Cache-warm fast path: read SessionIDs off the ring without materialising summaries.
	if v, ok := s.recentCache.Load(jobID); ok {
		entry := v.(*recentCacheEntry)
		entry.mu.RLock()
		if entry.warm {
			limit := n
			if limit > entry.count {
				limit = entry.count
			}
			// Empty ring: skip the allocs; release the read lock before returning.
			if limit == 0 {
				entry.mu.RUnlock()
				return nil
			}
			out := make([]string, 0, limit)
			seen := make(map[string]struct{}, limit)
			for i := 0; i < limit; i++ {
				sid := entry.ringRead(i).SessionID
				if sid == "" {
					continue
				}
				if _, dup := seen[sid]; dup {
					continue
				}
				seen[sid] = struct{}{}
				out = append(out, sid)
			}
			entry.mu.RUnlock()
			return out
		}
		entry.mu.RUnlock()
	}
	// Cold path: cold misses are rare (warmCache lazy-fills on first List/Recent).
	rows := s.List(jobID, n, time.Time{})
	// No runs: skip the allocs, mirroring the warm-path empty-ring guard.
	if len(rows) == 0 {
		return nil
	}
	out := make([]string, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for i := range rows {
		sid := rows[i].SessionID
		if sid == "" {
			continue
		}
		if _, dup := seen[sid]; dup {
			continue
		}
		seen[sid] = struct{}{}
		out = append(out, sid)
	}
	return out
}

// Get returns the full CronRun for runID under jobID, or (nil, error)
// when missing / corrupt. ErrCorruptRun signals "file present but
// unusable" so the caller can render a "this run's record is broken"
// placeholder instead of a 404.
func (s *runStore) Get(jobID, runID string) (*CronRun, error) {
	if s == nil || !s.layout.Enabled() {
		return nil, fs.ErrNotExist
	}
	if !IsValidID(jobID) || !IsValidID(runID) {
		return nil, fs.ErrNotExist
	}
	path := filepath.Join(s.rootDir(), jobID, runID+".json")
	return s.readRun(path)
}

// parseRunBytes is the ReadFile + size-cap + json.Unmarshal tail used by
// readRunNoLstat, keeping over-cap / unmarshal error wrapping identical to
// parseRunFromFile.
func (s *runStore) parseRunBytes(path string) (*CronRun, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decodeRunBytes(data, s.maxRunBytes)
}

// readAllInto reads f to EOF, appending into the supplied prefix-allocated
// buffer. Mirrors io.ReadAll's loop but lets the caller pre-size based on
// Fstat to avoid repeated re-grows on the typical ~2KB run record.
func readAllInto(f *os.File, buf []byte) ([]byte, error) {
	return readAllIntoReader(f, buf)
}

// DeleteJob removes the entire runs/<jobID>/ subtree. Idempotent: missing
// dir is a no-op. Does NOT delete ~/.claude/projects/<cwd>/<session_id>.jsonl
// (user-facing claude session logs).
//
// runs/ and cron_jobs.json have no atomic transaction spanning both; in
// withJobByPrefix this runs BEFORE cron_jobs.json is saved. A crash in that
// window is benign: the job reloads with empty history and repopulates runs/.
// The reverse order would orphan a runs/<jobID>/ subtree that trimAll (known
// jobs only) never reclaims, so "remove runs/ first" is deliberate; it also
// means a failed persist does not leak runs/ (#762).
func (s *runStore) DeleteJob(jobID string) {
	if !s.enabled() || jobID == "" {
		return
	}
	if !IsValidID(jobID) {
		return
	}
	lock := s.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()
	dir := filepath.Join(s.rootDir(), jobID)
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("cron run: delete job runs subtree failed", "dir", dir, "err", err)
	}
	s.cacheInvalidate(jobID)
	// Drop the layout's MkdirAll marker AND the per-job mutex: the marker so a
	// subsequent Append recreates the dir, the mutex so the map does not grow
	// without bound across create/delete churn (#971). Safe under the held
	// lock: a caller that already loaded THIS mutex still serialises on it; one
	// loading after the drop gets a fresh mutex — the benign "deleted job races
	// Append" edge from jobLock's godoc.
	s.layout.ForgetOwner(jobID)
}

// dropOrphanRun undoes ONE run-record write that lost the race against
// DeleteJob (#2479): finishRun calls it when its post-Append re-check finds
// the job gone, since trimAll (known jobs only) would never reclaim the file.
//
// Safe without a generation / tombstone: it removes exactly <runID>.json,
// then rmdir non-recursively (ENOTEMPTY = someone else owns the dir now, and
// is ignored), drops the layout marker so a later Append re-runs MkdirAll, and
// runs under jobLock(jobID) so it serialises with concurrent Appends. Job IDs
// are 8 crypto/rand bytes, so a same-ID rebuild is not a practical concern.
// Errors are logged, never returned.
func (s *runStore) dropOrphanRun(jobID, runID string) {
	if !s.enabled() || !IsValidID(jobID) || !IsValidID(runID) {
		return
	}
	lock := s.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()
	dir := filepath.Join(s.rootDir(), jobID)
	path := filepath.Join(dir, runID+".json")
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("cron run: drop orphan run record failed", "path", path, "err", err)
	}
	s.layout.ForgetOwner(jobID)
	s.cacheInvalidate(jobID)
	// Non-recursive on purpose: ENOTEMPTY means another writer owns the dir.
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) && !isDirNotEmpty(err) {
		slog.Warn("cron run: drop orphan runs dir failed", "dir", dir, "err", err)
	}
}

// runRecord pairs a marshalled payload with the summary of the very record it
// was marshalled from. Append carries the two together so the bytes written to
// disk and the row pushed into the read cache cannot describe different records.
//
// This replaced a source-anchor test that pinned Append's `summarySrc = &shrunk`
// rebinding by regex (#1079, #2547). The rebinding was needed because the
// truncate-and-retry block appeared twice — once for the preflight over-cap path
// and once post-marshal — so each copy had to remember to update both the
// payload and the cache source. One runRecord and one shrinkAndMarshal leaves a
// single line where the pairing is established.
//
// No test in this package can currently see the pairing go wrong: CronRunSummary
// carries RunID, JobID, State, Trigger, StartedAt, EndedAt, DurationMS,
// SessionID, ErrorClass, ReplayOf and CostUSD, none of which truncation changes.
// TestAppend_OversizeRetry_DiskAndCacheAgree compares the whole cached summary
// against the on-disk record's, so it arms itself the day a truncated field is
// added — which is the future the regex anchor was aimed at and could not have
// caught.
type runRecord struct {
	payload []byte
	summary CronRunSummary
}

// shrinkAndMarshal truncates the three unbounded string fields of a copy of run
// and marshals that copy, returning the payload and that copy's summary as one
// runRecord.
//
// fits is false when the truncated record still exceeds the cap; the payload is
// returned anyway so the caller can log its post-truncate size.
func (s *runStore) shrinkAndMarshal(run *CronRun) (rec runRecord, fits bool, err error) {
	shrunk := *run
	shrunk.Result = truncateWithSuffix(shrunk.Result, maxRetryFieldRunes)
	shrunk.Prompt = truncateWithSuffix(shrunk.Prompt, maxRetryFieldRunes)
	shrunk.ErrorMsg = truncateWithSuffix(shrunk.ErrorMsg, maxRetryFieldRunes)
	// ResultBytes is the STORED byte count and must match the truncated Result on disk (#2016).
	shrunk.ResultBytes = len(shrunk.Result)
	data, err := marshalRunPooled(&shrunk)
	if err != nil {
		return runRecord{}, false, err
	}
	if int64(len(data)) > s.maxRunBytes {
		return runRecord{payload: data}, false, nil
	}
	return runRecord{payload: data, summary: shrunk.summary()}, true, nil
}

// isDirNotEmpty reports whether err is the rmdir-on-non-empty-directory
// failure (ENOTEMPTY on Linux/macOS; some platforms report EEXIST).
func isDirNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}

// now is the runStore's time source: the injected clock, or wall-clock time when
// none was wired (every production construction path). Mirrors Scheduler.now().
func (s *runStore) now() time.Time {
	if s == nil || s.clock == nil {
		return time.Now()
	}
	return s.clock.Now()
}
