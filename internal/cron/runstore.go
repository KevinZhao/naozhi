package cron

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// runStore persists CronRun records under runsRoot (filepath.Dir(StorePath)+"/runs"):
//
//	runs/<jobID>/<run_id>.json    # one record per run; ~2KB typical
//
// Per-job file ops are serialised by a sync.Map of *sync.Mutex keyed on jobID;
// WriteFileAtomic relies on rename uniqueness. Scheduler.storeMu is NOT shared.
//
// Lock hierarchy: Scheduler.s.mu > runStore.jobLock(jobID) > recentCacheEntry.mu.
// 已持 entry.mu 时禁止再获取 jobLock 或 s.mu（cacheGet 走"释放-重取"模式）。
// Errors are surfaced via slog, never returned: cron must not block on history failure.
type runStore struct {
	root string
	// keepCount / keepWindow / maxRunBytes 在 newRunStore 之后不可变，读取无需锁。
	keepCount   int
	keepWindow  time.Duration
	maxRunBytes int64
	jobLocks    sync.Map // jobID -> *sync.Mutex
	// jobDirEnsured 记录 Append 已为该 jobID 跑过 MkdirAll，省掉每次 Append 的 lstat+mkdir。
	// sync.Map 原子读写；MkdirAll 幂等，失败时删掉 cache 项让下次重试。
	jobDirEnsured sync.Map // jobID -> struct{}
	disabled      bool     // true when StorePath is empty (tests / no-persist)
	enableTrimGC  bool     // true in production; tests can disable for determinism

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

	// writeFailedTotal counts CronRun WriteFileAtomic failures, split so operators
	// can distinguish ENOSPC from EACCES / IO errors. Append cannot return errors,
	// so this counter plus the Error log is the only operator-visible signal (#1338).
	writeFailedDiskFullTotal atomic.Int64
	writeFailedOtherTotal    atomic.Int64

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

// WriteFailedTotals returns the cumulative count of CronRun WriteFileAtomic
// failures since process start, split by failure class (monotonic counters).
// diskFull is osutil.IsDiskFull (ENOSPC + EDQUOT); other is every other write
// failure. Returns (0, 0) when s is nil or disabled.
func (s *runStore) WriteFailedTotals() (diskFull, other int64) {
	if s == nil {
		return 0, 0
	}
	return s.writeFailedDiskFullTotal.Load(), s.writeFailedOtherTotal.Load()
}

// enabled reports whether this runStore will persist / serve run history,
// folding the nil receiver and the disabled flag (StorePath empty) into one
// predicate so callers do not hand-roll `s.runStore != nil` (#993).
func (s *runStore) enabled() bool {
	return s != nil && !s.disabled
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
		return &runStore{disabled: true}
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
	// Abs also cleans `..` / `.` segments; if it fails (CWD gone) Clean still
	// strips traversal.
	storeAbs, err := filepath.Abs(storePath)
	if err != nil {
		slog.Warn("cron run: storePath Abs failed; falling back to Clean", "path", storePath, "err", err)
		storeAbs = filepath.Clean(storePath)
	}
	root := filepath.Join(filepath.Dir(storeAbs), "runs")
	// runs/ 根目录主动建为 0o700：否则继承父目录权限（通常 0o755），同机其他
	// OS 用户可枚举 jobID。失败仅 Warn，后续 Append 仍会 MkdirAll 子目录。
	if err := os.MkdirAll(root, 0o700); err != nil {
		slog.Warn("cron run: mkdir root failed", "root", root, "err", err)
	}
	// MkdirAll honours perm only on directories it creates, so a pre-existing
	// 0o755/0o777 runs/ keeps its mode and leaks job existence + content to other
	// OS users. Chmod the leaf to 0o700; log + continue because bind-mounted
	// container roots may not be chmod-able. The Lstat below is the
	// authoritative symlink guard; this is only perm-tightening (#504).
	if fi, err := os.Lstat(root); err == nil && fi.Mode()&fs.ModeSymlink == 0 && fi.IsDir() {
		if perm := fi.Mode().Perm(); perm != 0o700 {
			if cerr := os.Chmod(root, 0o700); cerr != nil {
				slog.Warn("cron run: chmod runs root to 0700 failed",
					"root", root, "had_mode", perm.String(), "err", cerr)
			} else {
				slog.Info("cron run: corrected runs root mode to 0700",
					"root", root, "had_mode", perm.String())
			}
		}
	}
	// MkdirAll does not error when the path already exists as a symlink to a
	// directory, so a pre-created `<dataDir>/runs -> /etc` would land every
	// CronRun JSON outside the data dir. Reject anything that is not a plain
	// directory and disable the store (#825).
	if fi, err := os.Lstat(root); err == nil {
		if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
			slog.Error("cron run: runs/ is a symlink or non-directory; disabling store",
				"root", root, "mode", fi.Mode().String())
			return &runStore{disabled: true}
		}
	}
	return &runStore{
		root:         root,
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
	if v, ok := s.jobLocks.Load(jobID); ok {
		return v.(*sync.Mutex)
	}
	m := &sync.Mutex{}
	actual, _ := s.jobLocks.LoadOrStore(jobID, m)
	return actual.(*sync.Mutex)
}

// assertJobLockHeld logs a warning when jobLock(jobID) is currently free —
// the signature of a caller that violated the *Locked-suffix contract.
// Best-effort: it warns rather than panics because cron history must never
// take the scheduler down (#696, #694), false negatives under contention are
// accepted, and the TryLock probe only runs under `go test` since it sits on
// the Append hot path (#961).
func (s *runStore) assertJobLockHeld(jobID string) {
	if !testing.Testing() {
		return
	}
	lock := s.jobLock(jobID)
	if lock.TryLock() {
		lock.Unlock()
		slog.Warn("cron runstore: jobLock not held by caller; *Locked-suffix contract violated",
			"job_id", jobID)
	}
}

// ensureJobDir 确保 dir 已存在；jobDirEnsured 命中后跳过 MkdirAll + root fsync
// 的 syscall（长寿 job 每次 Append 的 lstat+mkdir 是纯浪费）。缓存只是省
// syscall，不是正确性保证：MkdirAll 幂等，失败时删掉 cache 项让下次重试。
func (s *runStore) ensureJobDir(jobID, dir string) error {
	// Symlink guard mirrors newRunStore's root Lstat: MkdirAll does not error on
	// a symlink-to-directory, and the filepath.Rel check at the call site never
	// follows on-disk symlinks (#1504). It MUST run on every Append, not just the
	// cache-miss path, or a swap to a symlink after the first Append would
	// bypass it; the cache only skips the idempotent MkdirAll + root fsync (#1968).
	if fi, err := os.Lstat(dir); err == nil {
		if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
			slog.Error("cron run: per-job runs dir is a symlink or non-directory; refusing append",
				"dir", dir, "mode", fi.Mode().String(), "job_id", jobID)
			// Drop the stale "ensured" marker so a later restore of the dir is re-validated.
			s.jobDirEnsured.Delete(jobID)
			return fmt.Errorf("cron run: per-job dir %q is not a plain directory", dir)
		}
	}
	if _, ok := s.jobDirEnsured.Load(jobID); ok {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		// 不写入 cache：让下次 Append 重试。
		return err
	}
	// The new runs/<jobID>/ entry lives in runs/ and is not durable until the
	// root is fsynced; WriteFileAtomic only fsyncs the file's immediate parent,
	// so a crash could orphan the record. Fsync the root once per fresh subdir
	// (cache-miss path only). SyncDir swallows soft errors (#976).
	if s.root != "" {
		if err := osutil.SyncDir(s.root); err != nil {
			// Non-fatal: only crash-durability of the directory entry is degraded.
			slog.Debug("cron run: runs root fsync skipped", "root", s.root, "err", err)
		}
	}
	s.jobDirEnsured.Store(jobID, struct{}{})
	return nil
}

// Append writes one run record to disk and trims the per-job ring.
// Errors are logged, never returned: cron must not block history failure.
func (s *runStore) Append(run *CronRun) {
	if s == nil || s.disabled || run == nil || run.JobID == "" || run.RunID == "" {
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
	// run outside jobLock (#549). summarySrc is rebound to &shrunk on the
	// over-cap path so the cache row matches the truncated on-disk record (#1079).
	// Preflight: when Result+Prompt+ErrorMsg alone overshoot the cap minus a fixed
	// headroom, skip the doomed first marshal; the post-marshal gate stays authoritative (#1111).
	const fixedFieldsHeadroom = 1024
	preflightOverCap := s.maxRunBytes > fixedFieldsHeadroom &&
		int64(len(run.Result)+len(run.Prompt)+len(run.ErrorMsg)) >
			s.maxRunBytes-fixedFieldsHeadroom
	var data []byte
	var err error
	summarySrc := run
	if preflightOverCap {
		// Distinct message so preflight over-cap is distinguishable from the post-marshal retry.
		slog.Warn("cron run: preflight over-cap: truncating result/prompt directly (skipping full marshal)",
			"job_id", run.JobID, "run_id", run.RunID,
			"preflight_bytes", len(run.Result)+len(run.Prompt)+len(run.ErrorMsg),
			"cap", s.maxRunBytes)
		shrunk := *run
		shrunk.Result = truncateWithSuffix(shrunk.Result, maxRetryFieldRunes)
		shrunk.Prompt = truncateWithSuffix(shrunk.Prompt, maxRetryFieldRunes)
		shrunk.ErrorMsg = truncateWithSuffix(shrunk.ErrorMsg, maxRetryFieldRunes)
		// ResultBytes is the STORED byte count and must match the truncated Result on disk (#2016).
		shrunk.ResultBytes = len(shrunk.Result)
		data2, err2 := marshalRunPooled(&shrunk)
		if err2 != nil || int64(len(data2)) > s.maxRunBytes {
			retryBytes := -1
			if err2 == nil {
				retryBytes = len(data2)
			}
			s.historyDropTotal.Add(1)
			slog.Warn("cron run: retry marshal also exceeded cap; run record dropped",
				"job_id", run.JobID,
				"run_id", run.RunID,
				"retry_err", err2,
				"retry_bytes", retryBytes,
				"cap", s.maxRunBytes)
			return
		}
		data = data2
		summarySrc = &shrunk
	} else {
		data, err = marshalRunPooled(run)
		if err != nil {
			slog.Warn("cron run: marshal failed", "job_id", run.JobID, "run_id", run.RunID, "err", err)
			return
		}
	}
	if int64(len(data)) > s.maxRunBytes {
		slog.Warn("cron run: payload exceeds size cap; truncating result/prompt and retrying",
			"job_id", run.JobID, "run_id", run.RunID, "bytes", len(data), "cap", s.maxRunBytes)
		// 退化路径：把 Result 砍到极短，重新 marshal。Prompt 亦同。
		// 这里不返回 — 一定要落盘一条记录，UI 才能看到 "曾有这么一条 run"。
		shrunk := *run
		shrunk.Result = truncateWithSuffix(shrunk.Result, maxRetryFieldRunes)
		shrunk.Prompt = truncateWithSuffix(shrunk.Prompt, maxRetryFieldRunes)
		shrunk.ErrorMsg = truncateWithSuffix(shrunk.ErrorMsg, maxRetryFieldRunes)
		// Recompute ResultBytes to match the truncated Result written to disk (#2016).
		shrunk.ResultBytes = len(shrunk.Result)
		if data2, err2 := marshalRunPooled(&shrunk); err2 == nil && int64(len(data2)) <= s.maxRunBytes {
			data = data2
			// Cache row must match the truncated record that landed on disk (#1079).
			summarySrc = &shrunk
		} else {
			// err2 may be nil when the truncated payload still exceeds maxRunBytes
			// (metadata alone over cap), so log both err2 and the post-truncate size.
			retryBytes := -1
			if err2 == nil {
				retryBytes = len(data2)
			}
			s.historyDropTotal.Add(1)
			slog.Warn("cron run: retry marshal also exceeded cap; run record dropped",
				"job_id", run.JobID,
				"run_id", run.RunID,
				"retry_err", err2,
				"retry_bytes", retryBytes,
				"cap", s.maxRunBytes)
			return
		}
	}

	// The disk write runs OUTSIDE jobLock: each Append writes a unique
	// <runID>.json (rename-atomic), so concurrent Appends do not collide, and
	// holding the lock across fsync+rename serialised every Append behind a slow
	// disk (#1335). The warmCache-reads-new-file-before-cacheHeadPush interleave
	// is neutralised by the RunID dedup inside cacheHeadPush.
	dir := filepath.Join(s.root, run.JobID)
	// Defense-in-depth containment check mirroring readRun's root guard:
	// IsValidID already rejects non-hex, but a future caller bypassing it must
	// not be able to escape s.root via the join (#484).
	if rel, relErr := filepath.Rel(s.root, dir); relErr != nil ||
		rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		slog.Error("cron run: refusing append outside runs root",
			"root", s.root, "dir", dir, "rel", rel, "err", relErr,
			"job_id", run.JobID, "run_id", run.RunID)
		return
	}
	if s.appendPreWriteHook != nil {
		s.appendPreWriteHook(run.JobID)
	}
	if err := s.ensureJobDir(run.JobID, dir); err != nil {
		slog.Warn("cron run: mkdir failed", "dir", dir, "err", err)
		return
	}
	path := filepath.Join(dir, run.RunID+".json")
	if err := osutil.WriteFileAtomic(path, data, 0o600); err != nil {
		// Append cannot return an error (history is best-effort), so the counter
		// plus Error-level log is the only operator-visible signal (#1338).
		diskFull := osutil.IsDiskFull(err)
		if diskFull {
			s.writeFailedDiskFullTotal.Add(1)
		} else {
			s.writeFailedOtherTotal.Add(1)
		}
		slog.Error("cron run: write failed; run record dropped",
			"path", path, "err", err, "disk_full", diskFull,
			"job_id", run.JobID, "run_id", run.RunID)
		return
	}

	// Cache push + trim run under jobLock so cacheHeadPush / cacheGetBefore /
	// trimJobLocked stay serialised per job; the critical section is now
	// O(µs) ring updates, not O(fsync). cacheHeadPush no-ops on a cold cache.
	lock := s.jobLock(run.JobID)
	lock.Lock()
	defer lock.Unlock()
	s.cacheHeadPush(run.JobID, summarySrc.summary())
	if s.enableTrimGC {
		// One time.Now() shared by skipAppendTrim and trimJobLocked.
		now := time.Now()
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
	if s == nil || s.disabled || jobID == "" {
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
	if s == nil || s.disabled || jobID == "" {
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
	if s == nil || s.disabled {
		return nil, fs.ErrNotExist
	}
	if !IsValidID(jobID) || !IsValidID(runID) {
		return nil, fs.ErrNotExist
	}
	path := filepath.Join(s.root, jobID, runID+".json")
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
	if s == nil || s.disabled || jobID == "" {
		return
	}
	if !IsValidID(jobID) {
		return
	}
	lock := s.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()
	dir := filepath.Join(s.root, jobID)
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("cron run: delete job runs subtree failed", "dir", dir, "err", err)
	}
	// Drop the MkdirAll cache so a subsequent Append recreates the dir.
	s.jobDirEnsured.Delete(jobID)
	s.cacheInvalidate(jobID)
	// Reclaim the per-job mutex too, or the map grows without bound across
	// create/delete churn (#971). Safe under the held lock: a caller that already
	// loaded THIS mutex still serialises on it; one loading after Delete gets a
	// fresh mutex — the benign "deleted job races Append" edge from jobLock's godoc.
	s.jobLocks.Delete(jobID)
}

// dropOrphanRun undoes ONE run-record write that lost the race against
// DeleteJob (#2479): finishRun calls it when its post-Append re-check finds
// the job gone, since trimAll (known jobs only) would never reclaim the file.
//
// Safe without a generation / tombstone: it removes exactly <runID>.json,
// then rmdir non-recursively (ENOTEMPTY = someone else owns the dir now, and
// is ignored), drops jobDirEnsured so a later Append re-runs MkdirAll, and
// runs under jobLock(jobID) so it serialises with concurrent Appends. Job IDs
// are 8 crypto/rand bytes, so a same-ID rebuild is not a practical concern.
// Errors are logged, never returned.
func (s *runStore) dropOrphanRun(jobID, runID string) {
	if s == nil || s.disabled || !IsValidID(jobID) || !IsValidID(runID) {
		return
	}
	lock := s.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()
	dir := filepath.Join(s.root, jobID)
	path := filepath.Join(dir, runID+".json")
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("cron run: drop orphan run record failed", "path", path, "err", err)
	}
	s.jobDirEnsured.Delete(jobID)
	s.cacheInvalidate(jobID)
	// Non-recursive on purpose: ENOTEMPTY means another writer owns the dir.
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) && !isDirNotEmpty(err) {
		slog.Warn("cron run: drop orphan runs dir failed", "dir", dir, "err", err)
	}
}

// isDirNotEmpty reports whether err is the rmdir-on-non-empty-directory
// failure (ENOTEMPTY on Linux/macOS; some platforms report EEXIST).
func isDirNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}
