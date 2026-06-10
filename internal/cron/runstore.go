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
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// runStore persists CronRun records to disk. Layout (rooted at runsRoot,
// derived from filepath.Dir(StorePath)+"/runs"):
//
//	runs/
//	    index.json                # cross-job recent-list cache (P2 use)
//	    <jobID>/
//	        <run_id>.json         # one record per run; ~2KB typical
//
// Concurrency: per-job file ops are serialised by a fine-grained sync.Map
// of *sync.Mutex keyed on jobID. WriteFileAtomic still relies on rename
// uniqueness (os.CreateTemp). The package-level cron_jobs.json mutex
// (Scheduler.storeMu) is NOT shared with this store: the two write to
// different files and serialising would only inflate latency.
//
// R238-ARCH-12 (#755) proposed extracting runStore into its own
// internal/cron/runs sub-package so runstore.go (~830 LOC) and its
// independent lock hierarchy stop bloating the cron package surface.
// Deferred — extraction would force a sweep across:
//
//   - The 30+ test files in internal/cron/ that touch runStore
//     internals (jobLock, recentCacheEntry, ringPushHead, etc.).
//   - Scheduler fields (runStore + persistence wiring) that would
//     need a public Store interface plus an adapter for the
//     skipAppendTrim → trimJobLocked → cacheTrimAfterDisk back-edges.
//   - The CronRun / CronRunSummary value types currently shared
//     between scheduler.go (run construction) and runstore.go
//     (persistence) — extracting would force these into a third
//     shared package or duplicate the schema.
//
// The sub-package split is tracked in the broader cron-sysession-merge
// refactor (RFC docs/rfc/cron-sysession-merge.md Phase D-prep): the
// extraction is gated on the runtelemetry common-event-layer landing,
// without which runstore.go's slog/metrics calls would have to
// re-import cron just to talk back. Until then the file is well-
// modularised within cron via the lock-hierarchy comment above and
// scheduler_persist.go owning the cross-store coordination.
//
// Lock hierarchy（R234-GO-7 文档化锁层级）：
//
//	Scheduler.s.mu  >  runStore.jobLock(jobID)  >  recentCacheEntry.mu
//
// 任何路径若已持 entry.mu，禁止再去获取 jobLock 或 s.mu，否则将与
// Append（先 jobLock 后 entry.mu）出现死锁。当前 cacheGet 通过先释放
// entry.mu 再走 warmCache → jobLock 的"释放-重取"模式遵守此层级；
// 新增任何持锁路径必须先复核此约束再 review。
//
// Errors are surfaced via slog rather than propagated to callers (cron
// never blocks on history failure — RFC §4.2). The exception: GC's
// cumulative result is logged but not aborted on a single file failure.
type runStore struct {
	root string
	// keepCount / keepWindow / maxRunBytes 在 newRunStore 之后即不可变，
	// 任何后续 read 不需要锁保护。若将来引入运行时调整 API，必须先把
	// 三者改为 atomic.Int64 / atomic.Value 之后才能放开 store 之外的
	// 写入路径。R234-GO-13。
	keepCount   int
	keepWindow  time.Duration
	maxRunBytes int64
	jobLocks    sync.Map // jobID -> *sync.Mutex
	// jobDirEnsured 记录 Append 已经为该 jobID 跑过 MkdirAll 至少一次。
	// 长寿 cron job（每 1m 触发，存活若干天）每次 Append 都做 lstat+mkdir
	// 是纯浪费 syscall — 除非 operator 手动 rm -rf 该目录，目录一旦建立就
	// 不会消失。R246-GO-4 / R232-PERF-8 同根因（对 store dir 已用相似
	// storeDirOnce 模式）。
	//
	// Concurrency: sync.Map 自带原子读写。两个 goroutine 同时 Append 同
	// jobID 时仅一个 Store，多余的 LoadOrStore 命中自然降级。MkdirAll 本身
	// 也是幂等的，作为 fallback 自洽。
	//
	// Cache miss 路径：sync.Map.LoadOrStore 拿到 false 后跑 MkdirAll；失败
	// 则 Delete cache entry 让下次重试，避免一次 transient 错误永久毒化。
	jobDirEnsured sync.Map // jobID -> struct{}
	disabled      bool     // true when StorePath is empty (tests / no-persist)
	enableTrimGC  bool     // true in production; tests can disable for determinism

	// recentCache memoises the newest-N summaries per job so the dashboard
	// list endpoint (1 Hz poll × 50 jobs) does not perform 50× ReadDir +
	// 250× ReadFile per second. Cache is populated by Append (push to head)
	// and trimmed by trimJobLocked; List / Recent serve from it without IO.
	// Cache miss falls back to disk and lazily warms.
	//
	// Concurrency: each cache entry is owned by the same jobLock as the
	// on-disk subtree, so reads under jobLock are race-free with writes.
	// We do not expose the slice itself — callers always receive a fresh
	// copy so dashboard handlers can sort / filter without mutating cache.
	recentCache sync.Map // jobID -> *recentCacheEntry

	// cacheGetPostWarmHook is a test-only seam invoked by cacheGet right
	// after warmCache returns and before the post-warm re-Load. Lets tests
	// deterministically interleave a concurrent cacheInvalidate inside the
	// warmCache→re-Load window (R20260610-GO-007 / #2000). Always nil in
	// production; set only from tests, before the store is shared across
	// goroutines.
	cacheGetPostWarmHook func(jobID string)

	// writeFailedTotal counts CronRun WriteFileAtomic failures (disk full,
	// permission denied, ENOSPC, etc.). R20260527122801-CR-18 (#1338): Append
	// historically only slog.Warn'd on a write failure, so an operator whose
	// disk filled mid-run had no actionable signal until they noticed
	// runs/ stopped growing. We can't add a top-level metric because this
	// package owns the runstore (cron-local concern) — expose a counter
	// here that /health and tests can read, and bump severity to Error so
	// log scrapers alert. The split between writeFailedDiskFullTotal and
	// writeFailedOtherTotal lets operators distinguish "ENOSPC, free up
	// disk" from "EACCES / IO error, investigate the storage backend".
	writeFailedDiskFullTotal atomic.Int64
	writeFailedOtherTotal    atomic.Int64

	// historyDropTotal counts CronRun records that Append dropped entirely
	// because even the truncated retry payload still exceeded maxRunBytes
	// (or the retry marshal itself failed). R249-CR-21 (#964): the drop
	// path previously only slog.Warn'd, so operators had no numeric signal
	// and could not triangulate started/ended/dropped. Same package-local
	// counter pattern as writeFailed*Total — surfaced via HistoryDropTotal
	// for /health + tests. CronRunStartedTotal − CronRunEndedTotal should
	// reconcile against this drop count when a run finished but produced no
	// history row.
	historyDropTotal atomic.Int64

	// cacheStaleEvictionTotal counts recentCache rows that cacheTrimAfterDisk
	// evicted using its EndedAt/StartedAt time-source approximation rather
	// than the authoritative disk mtime trimJobLocked uses. R249-CR-19 (#962):
	// the < ~1s pathological divergence between the two time sources was
	// godoc-documented but had no runtime observability. A non-trivial,
	// growing delta here (relative to disk-side trims) is the signal that the
	// approximation is evicting cache rows whose disk files are still kept —
	// the exact divergence the godoc warned about.
	cacheStaleEvictionTotal atomic.Int64
}

// HistoryDropTotal returns the cumulative count of CronRun records Append
// dropped because the truncated retry payload still exceeded maxRunBytes.
// Monotonically non-decreasing; returns 0 when s is nil or disabled.
// R249-CR-21 (#964).
func (s *runStore) HistoryDropTotal() int64 {
	if s == nil {
		return 0
	}
	return s.historyDropTotal.Load()
}

// CacheStaleEvictionTotal returns the cumulative count of recentCache rows
// evicted by cacheTrimAfterDisk's approximate time source. Monotonically
// non-decreasing; returns 0 when s is nil or disabled. R249-CR-19 (#962).
func (s *runStore) CacheStaleEvictionTotal() int64 {
	if s == nil {
		return 0
	}
	return s.cacheStaleEvictionTotal.Load()
}

// WriteFailedTotals returns the cumulative count of CronRun WriteFileAtomic
// failures since process start, split by failure class. Stable counter
// semantics: monotonically non-decreasing; callers diff snapshots over
// time to compute rates.  R20260527122801-CR-18 (#1338).
//
// diskFull counts errors classified by osutil.IsDiskFull (ENOSPC + EDQUOT
// today); other counts every other write failure (EACCES, EIO, broken
// symlink under runs/, etc.). Returns (0, 0) when s is nil or disabled.
func (s *runStore) WriteFailedTotals() (diskFull, other int64) {
	if s == nil {
		return 0, 0
	}
	return s.writeFailedDiskFullTotal.Load(), s.writeFailedOtherTotal.Load()
}

// enabled reports whether this runStore will actually persist / serve run
// history. It folds the two historically-separate "off" signals — a nil
// *runStore receiver and the disabled flag (StorePath empty) — into one
// predicate so callers stop hand-rolling `s.runStore != nil` in some
// places and relying on the method-internal `s.disabled` guard in others.
// R249-ARCH-29 (#993): that nil-vs-disabled split meant a no-persist
// scheduler (disabled runStore, non-nil pointer) still spun up the
// cold-start GC goroutine / RecentSessionIDs fan-out that an external
// `!= nil` check could not skip. Every runStore method already short-
// circuits on `s == nil || s.disabled`, so this is purely the external
// gate that mirrors that internal contract — null-object semantics
// without introducing a separate noop type.
func (s *runStore) enabled() bool {
	return s != nil && !s.disabled
}

// User-configurable defaults (DefaultRunsKeepCount / DefaultRunsKeepWindow)
// and hard schema caps (MaxRunRecordBytes) live in limits.go alongside the
// other cron-trust-boundary constants — see R247-CR-12 / R247-CR-20 (#598)
// for the rationale.

// ErrCorruptRun is returned when a run JSON file fails to parse or
// exceeds the size cap. Treated identically to "missing": list APIs
// skip the entry, GC removes it.
var ErrCorruptRun = errors.New("cron run: corrupt or oversize record")

// newRunStore constructs a runStore rooted at <storePath dir>/runs.
// storePath="" disables the store (List returns empty, Append no-ops);
// callers can pass a Scheduler in tests without wiring up a tempdir.
//
// R245-SEC-1 (#825): the root path is normalised via filepath.Abs +
// filepath.Clean so a storePath containing `..` segments cannot escape
// its data dir (e.g. "/data/x/../../etc/cron.json" would otherwise
// produce a runs/ tree at "/data/x/../../etc/runs"). After mkdir, we
// Lstat the result — if the runs dir is a symlink (or some other
// non-directory), refuse to use the store: an operator-controlled
// runs/ symlink could redirect the entire run-history tree at a
// sensitive directory and any subsequent Append would write CronRun
// JSON over arbitrary files.
//
// R241-ARCH-8 (#512): optional maxBytesOpt overrides the default
// MaxRunRecordBytes per-record cap. Passing it brings constructor
// signature in parity with keepCount / keepWindow, both of which are
// already tunable. Variadic keeps the existing 3-arg call sites in
// scheduler.go and tests source-compatible — a missing or non-positive
// value falls back to MaxRunRecordBytes so the production caller sees
// no behaviour change.
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
	// filepath.Abs already cleans the path, normalising any `..` /  `.` /
	// double-slash segments. If Abs fails (CWD missing — extremely rare
	// outside containers in error states) fall back to Clean so we still
	// remove `..` traversal even when we can't fully resolve.
	storeAbs, err := filepath.Abs(storePath)
	if err != nil {
		slog.Warn("cron run: storePath Abs failed; falling back to Clean", "path", storePath, "err", err)
		storeAbs = filepath.Clean(storePath)
	}
	root := filepath.Join(filepath.Dir(storeAbs), "runs")
	// R234-SEC-4: 主动创建 runs/ 根目录并设 0o700。原先只在 Append 时
	// 创建 runs/<jobID>/ 子目录用 0o700，而 runs/ 自身继承父目录权限
	// （通常 0o755），同机器其他 OS 用户可枚举 jobID 列表，泄露 cron
	// 任务存在与数量。失败仅记录 Warn — 后续 Append 仍会在子目录路径
	// 上 MkdirAll，不影响功能。
	if err := os.MkdirAll(root, 0o700); err != nil {
		slog.Warn("cron run: mkdir root failed", "root", root, "err", err)
	}
	// R247-SEC-12 (#504): MkdirAll honours `perm` only on directories it
	// actually creates — a pre-existing runs/ tree (e.g. laid down by a
	// prior version with 0o755, or by an attacker who racd ahead of
	// startup) keeps whatever mode it had. The directory carries cron
	// run JSON files that include script source, env values, and stdout
	// summaries; world-readable parent dirs leak both the existence of
	// scheduled jobs and their content to other OS users on the same
	// host. Chmod the leaf to the contractual 0o700 so a pre-created
	// 0o755 / 0o777 dir is corrected on next startup. We log + continue
	// rather than fail because operators sometimes run naozhi inside
	// containers where the bind-mount root cannot be chmod'd by the
	// running uid (NoNewPrivileges, read-only rootfs); a hard fail would
	// brick the whole cron subsystem there. The Lstat check below is the
	// authoritative symlink/non-directory guard that protects against
	// the path-redirect attack — Chmod is only the perm-tightening step.
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
	// R245-SEC-1 (#825): refuse to attach to a runs/ that is a symlink or
	// other non-directory. MkdirAll does not error when the path already
	// exists as a symlink to a directory, so without this Lstat an
	// attacker (or accidental operator action) who pre-created
	// `<dataDir>/runs` as a symlink to `/etc` would have all subsequent
	// CronRun JSONs land outside the data dir. Lstat reports the link
	// itself; we reject anything that's not a plain directory and disable
	// the store so Append/List become safe no-ops rather than write to
	// the wrong tree.
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
// reclaimed by DeleteJob (R249-ARCH-3 / #971), so the live set tracks the
// live job set rather than every jobID that has ever existed; a deleted job
// racing a concurrent Append on the very same ID is the same edge handled
// by the runningJobs sync.Map.
func (s *runStore) jobLock(jobID string) *sync.Mutex {
	if v, ok := s.jobLocks.Load(jobID); ok {
		return v.(*sync.Mutex)
	}
	m := &sync.Mutex{}
	actual, _ := s.jobLocks.LoadOrStore(jobID, m)
	return actual.(*sync.Mutex)
}

// assertJobLockHeld logs a warning when jobLock(jobID) is currently free,
// which — outside concurrent tests — is the unambiguous signature of a
// caller that violated the *Locked-suffix contract (forgot to acquire).
// Use from helpers whose godoc says "caller must hold jobLock".
//
// R242-CR-11 (#696) / R242-CR-7 (#694): the historical implementation
// `panic`'d on the contract miss. Two real production hazards:
//
//  1. skipAppendTrim called assertJobLockHeld BEFORE locking entry.mu, so
//     a panic propagated up through Append's `defer lock.Unlock()` for
//     jobLock and eventually crashed the process. The cron history path
//     is supposed to be best-effort — RFC §4.2 says cron must NOT block
//     on history failure — yet a contract bug elsewhere could still take
//     the whole scheduler down.
//  2. The TryLock+Unlock pair is observable contention from any goroutine
//     legitimately holding the lock, plus any future caller that forgets
//     to hold it gets a `panic` rather than a bounded recoverable log.
//
// The check is still best-effort: TryLock+Unlock is cheap (uncontended
// fast path) and the warn message includes the jobID so failures point
// straight at the offending caller. False negatives — another goroutine
// holds the lock, our caller doesn't, TryLock fails so we miss the bug
// — are accepted in exchange for catching the dominant "single-flight
// test caller forgot to lock" failure mode reliably. R236-GO-03 +
// R242-CR-11 (#696) + R242-CR-7 (#694).
//
// Tests that want hard-fail-on-contract-miss can wrap the slog handler
// to escalate; production stays alive.
//
// R249-CR-18 (#961): the TryLock+Unlock pair costs ~30 ns on the Append
// hot path (skipAppendTrim + trimJobLocked both call this on every
// invocation) and is a pure best-effort check — false negatives are
// already accepted, the production warn path has fired exactly zero
// times since R242-CR-11 / R242-CR-7 replaced the original panic. Gate
// the lock probe behind testing.Testing() so production processes pay
// only the function-call overhead while `go test` still gets the
// contract assertion. The field-name + signature stay so future
// callers' godoc references and any test fixtures that look up the
// method via reflection continue to work.
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

// ensureJobDir 确保 dir 已存在，缓存命中后跳过 syscall。
//
// R246-GO-4: 长寿 cron job 每次 Append 触发的 os.MkdirAll(0o700) 在 Linux
// 实质执行 lstat + (cond) mkdir；目录稳定存在期内 lstat 是纯浪费，且在
// jobLock 下串行化所有 Append。jobDirEnsured 缓存首次成功之后，后续
// Append 走 sync.Map.Load fast-path 命中即返回。Cache miss 路径 MkdirAll
// 失败时把 cache 项删掉，避免单次 transient EACCES 永久毒化（下次 Append
// 会重试）。MkdirAll 自身幂等，作为 fallback 是安全的；缓存用于减少
// syscall，不是正确性保证。
func (s *runStore) ensureJobDir(jobID, dir string) error {
	// R250531-SEC-4 (#1504): mirror the runs/ root Lstat guard (newRunStore,
	// line ~502) on the per-job subdir. MkdirAll does NOT error when dir
	// already exists as a symlink to a directory, so a local attacker who
	// pre-created runs/<hexJobID> as a symlink to /tmp/evil/ before the first
	// Append would have this job's run records land outside s.root. The
	// filepath.Rel guard at the call site is a pure path check that does not
	// follow on-disk symlinks, so it cannot catch this. Lstat reports the
	// link itself; reject anything that exists but is not a plain directory.
	//
	// R20260608133928-CR-4 (#1968): the symlink guard MUST run on EVERY Append,
	// not just the first. The previous shape gated the entire function behind
	// the jobDirEnsured cache, so an attacker who swapped runs/<jobID>/ for a
	// symlink AFTER the first Append (cache already populated) would bypass the
	// Lstat on every subsequent tick and have records land at the symlink
	// target. Lstat at ~0.017Hz (1min jobs) is negligible, so we always
	// re-verify; the cache only skips the (idempotent) MkdirAll + root fsync.
	if fi, err := os.Lstat(dir); err == nil {
		if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
			slog.Error("cron run: per-job runs dir is a symlink or non-directory; refusing append",
				"dir", dir, "mode", fi.Mode().String(), "job_id", jobID)
			// Drop any stale "ensured" marker so a later legitimate restore of
			// the directory is re-validated rather than served from cache.
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
	// R249-ARCH-10 (#976): the newly created runs/<jobID>/ subdirectory entry
	// lives in the runs/ root and is not durable until the root is fsynced.
	// WriteFileAtomic later fsyncs the file's immediate parent (runs/<jobID>/)
	// but never its grandparent, so a crash after Append could leave the run
	// record on disk while the directory entry pointing at its parent dir is
	// lost — the record becomes orphaned/unreadable on recovery. Fsync the
	// runs/ root once per fresh subdir creation to close the gap. SyncDir
	// swallows soft errors (e.g. FUSE backends that reject directory fsync),
	// so this never blocks Append on filesystems lacking the capability.
	// Best-effort: only run on the cache-miss (first-time) path, so the
	// steady-state fast-path above stays syscall-free.
	if s.root != "" {
		if err := osutil.SyncDir(s.root); err != nil {
			// Non-fatal: the subdir + its first record still landed; only
			// crash-durability of the directory entry is degraded. Log so
			// operators can correlate "runs dir fsync skipped" with any
			// post-crash missing-history reports.
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
		// jobID 历史上是 16-hex；非 hex 可能是测试 fixture / 篡改文件。
		// 拒绝 append 而非创建可疑目录。
		slog.Warn("cron run: skipping append with non-hex job_id", "job_id", run.JobID)
		return
	}

	// R247-PERF-10 (#549): Marshal + over-cap shrink retry are pure CPU on
	// the caller-owned *run; they do not touch any runStore-shared state.
	// Hoisting them above jobLock keeps a slow JSON encode (CronRun can
	// approach maxRunBytes for chatty jobs) from serialising a concurrent
	// Append on the same jobID. WriteFileAtomic + cacheHeadPush + trim
	// stay under jobLock — those are the steps that genuinely need
	// per-job mutex serialisation. summarySrc rebinding to &shrunk on the
	// over-cap path is preserved verbatim so the cache row stays in
	// lockstep with the on-disk truncated record (#1079 / R250-GO-16).
	//
	// R250-PERF-8 (#1111): pre-flight string-length sum BEFORE the first
	// marshal. The dominant size contributors are Result/Prompt/ErrorMsg
	// (each potentially many KB on chatty jobs); when their byte sum alone
	// already overshoots maxRunBytes minus a small fixed-fields headroom,
	// we KNOW the first marshal would just be discarded so the retry path
	// runs. Skip straight to the truncate variant in that case — saves
	// one full json.Marshal on the rare-but-expensive over-cap path. The
	// cheap len() sum is O(1) per field; a stray small-fields-but-large-
	// metadata edge falls through to the original two-marshal path so
	// correctness is preserved (the post-marshal len(data) > maxRunBytes
	// gate below remains the authoritative check).
	const fixedFieldsHeadroom = 1024
	preflightOverCap := s.maxRunBytes > fixedFieldsHeadroom &&
		int64(len(run.Result)+len(run.Prompt)+len(run.ErrorMsg)) >
			s.maxRunBytes-fixedFieldsHeadroom
	var data []byte
	var err error
	summarySrc := run
	if preflightOverCap {
		// Skip the speculative first marshal; produce the truncated copy
		// directly. R20260602-CR-2: use a distinct message so preflight
		// over-cap is distinguishable from the post-marshal retry path.
		slog.Warn("cron run: preflight over-cap: truncating result/prompt directly (skipping full marshal)",
			"job_id", run.JobID, "run_id", run.RunID,
			"preflight_bytes", len(run.Result)+len(run.Prompt)+len(run.ErrorMsg),
			"cap", s.maxRunBytes)
		shrunk := *run
		shrunk.Result = truncateWithSuffix(shrunk.Result, maxRetryFieldRunes)
		shrunk.Prompt = truncateWithSuffix(shrunk.Prompt, maxRetryFieldRunes)
		shrunk.ErrorMsg = truncateWithSuffix(shrunk.ErrorMsg, maxRetryFieldRunes)
		data2, err2 := marshalRunPooled(&shrunk)
		if err2 != nil || int64(len(data2)) > s.maxRunBytes {
			retryBytes := -1
			if err2 == nil {
				retryBytes = len(data2)
			}
			// R249-CR-21 (#964): bump the package-local drop counter so the
			// loss is observable as a metric, not just a log line.
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
		if data2, err2 := marshalRunPooled(&shrunk); err2 == nil && int64(len(data2)) <= s.maxRunBytes {
			data = data2
			// #1079: keep the cache push consistent with disk — the
			// truncated record is what landed on disk, so the summary
			// must reflect those truncated fields.
			summarySrc = &shrunk
		} else {
			// R246-CR-250: previously this branch swallowed the failure
			// silently — operators had no signal that a run record was
			// actually dropped. Emit a warn so the loss is auditable.
			// err2 may be nil when the truncated payload still exceeds
			// maxRunBytes (rare; means metadata alone is over cap), so
			// log both err2 and the post-truncate size to disambiguate.
			retryBytes := -1
			if err2 == nil {
				retryBytes = len(data2)
			}
			// R249-CR-21 (#964): mirror the preflight drop counter so both
			// drop paths feed the same metric.
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

	// R20260527122801-PERF-4 (#1335): hoist the disk write OUT of jobLock.
	// WriteFileAtomic is rename-atomic at the FS level — each Append writes
	// a unique <runID>.json so two concurrent Appends do NOT collide on the
	// destination path. Holding jobLock across the fsync+rename serialised
	// every Append on the same job behind a slow disk, even though the
	// per-call work is independent. ensureJobDir is also safe outside the
	// lock: os.MkdirAll is idempotent + concurrent-safe, and the
	// jobDirEnsured cache is a sync.Map.
	//
	// The interleave hazard this opens — warmCache reading the new file
	// from disk before our cacheHeadPush re-acquires the lock — is
	// neutralised by the RunID-dedup inside cacheHeadPush (see comment
	// there). cacheHeadPush + skipAppendTrim + trimJobLocked still run
	// under jobLock so the cache + trim cadence keep their per-job
	// serialisation contract.
	dir := filepath.Join(s.root, run.JobID)
	// R247-GO-8 (#484): defense-in-depth path-containment check before
	// MkdirAll. IsValidID above already rejects `..` / `/` characters in
	// run.JobID (hex-only), so a malicious value cannot escape s.root via
	// the join — but the asymmetry vs readRun's Lstat-based root guard
	// invited future regressions when a new caller path bypassed
	// IsValidID. Mirror the read-side guard by computing
	// filepath.Rel(s.root, dir) and rejecting any rel that escapes the
	// root. Cheap (pure path manipulation, no syscall) and only fires the
	// reject branch if a future change to ID validation slips a `..`
	// segment through. R247-GO-8.
	if rel, relErr := filepath.Rel(s.root, dir); relErr != nil ||
		rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		slog.Error("cron run: refusing append outside runs root",
			"root", s.root, "dir", dir, "rel", rel, "err", relErr,
			"job_id", run.JobID, "run_id", run.RunID)
		return
	}
	if err := s.ensureJobDir(run.JobID, dir); err != nil {
		slog.Warn("cron run: mkdir failed", "dir", dir, "err", err)
		return
	}
	path := filepath.Join(dir, run.RunID+".json")
	if err := osutil.WriteFileAtomic(path, data, 0o600); err != nil {
		// R20260527122801-CR-18 (#1338): bump a runstore-local counter so
		// /health (and tests) can surface the failure rate as an actionable
		// signal, and escalate slog severity from Warn → Error so log-based
		// alerting fires. Cron Append cannot return error to the caller
		// (RFC §4.2 — history is best-effort), so the counter + Error log
		// is the only operator-visible signal.
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

	// Push to recentCache head + run trim under jobLock so concurrent
	// cacheHeadPush + cacheGetBefore + trimJobLocked stay serialised
	// per-job. The disk write is already durable above; this critical
	// section is now O(few-µs) ring updates instead of O(fsync) IO.
	// Cache may not yet be warm — that's fine: cacheHeadPush no-ops then,
	// and the next Recent call lazy-warms via warmCache.
	// #1079: summarySrc points at the truncated copy on the over-cap retry
	// path so the cache row matches the on-disk truncated bytes.
	lock := s.jobLock(run.JobID)
	lock.Lock()
	defer lock.Unlock()
	s.cacheHeadPush(run.JobID, summarySrc.summary())
	if s.enableTrimGC {
		// R20260603-PERF-11: capture a single time.Now() so both
		// skipAppendTrim's window-cutoff check and trimJobLocked share the
		// same instant, eliminating a redundant vDSO call on the trim path.
		now := time.Now()
		if !s.skipAppendTrim(run.JobID, now) {
			s.trimJobLocked(run.JobID, now)
		}
	}
}

// maxRetryFieldRunes 是 over-cap retry 路径每个字段（Result/Prompt/ErrorMsg）
// 各自允许的最大 rune 数。三处共用同一上限是有意——保证退化路径单条记录的
// 字节数可估算（≤ 3 × runesToBytes(maxRetryFieldRunes) + 元数据），不易再次
// 触发 maxRunBytes。R234-CR-9。
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
	// R249-ARCH-1 (#969): clamp to the configured retention cap, not the
	// package default. SchedulerConfig.RunsKeepCount is now plumbed into
	// s.keepCount (NewScheduler → newRunStore), so an operator who raised
	// retention above DefaultRunsKeepCount (200) must be able to page the
	// extra rows; the old hardcoded clamp silently truncated every query at
	// 200. s.keepCount is always > 0 for an enabled store, so when retention
	// is left at the default this is identical to the prior behaviour.
	if limit > s.keepCount {
		limit = s.keepCount
	}

	// Cache fast-path: when before is zero (most common — Recent and the
	// first paginated page) and the entry is warm, return without IO.
	// R220-PERF-1.
	if before.IsZero() {
		if cached, ok := s.cacheGet(jobID, limit); ok {
			return cached
		}
	} else {
		// before-cutoff fast path (R243-PERF-5 / #810): when the cache
		// has not yet hit keepCount the ring holds every on-disk row,
		// so a filter walk over the cache is equivalent to a disk scan.
		// Falls through to disk once count == keepCount because trim
		// may have shed older rows the caller would otherwise miss.
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
// (newest first). Convenience wrapper over List with limit=n, before=zero —
// hits cache on warm path. R220-PERF-1.
func (s *runStore) Recent(jobID string, n int) []CronRunSummary {
	return s.List(jobID, n, time.Time{})
}

// RecentSessionIDs returns up to n distinct non-empty SessionID strings from
// the newest-first run history for jobID. Functionally equivalent to walking
// `Recent(jobID, n)` and reading the SessionID field of each entry, but avoids
// the per-row CronRunSummary value-copy that Recent's defensive `ringSnapshot`
// makes (CronRunSummary embeds Result []byte up to ~4 KB plus several
// allocated string fields). On the buildKnownSessionsSet hot path
// (#1285 / R20260527-PERF-6) the caller only needs the session IDs and a
// 50-job × 200-cap walk over Recent copies up to 10 000 summary structs of
// pure overhead. Cache-warm fast path stays O(min(n, count)) under entry.mu;
// cold path falls back to disk via List+filter so we never silently miss a
// session ID that lives only on disk.
//
// Returns a fresh slice; safe to retain. Empty (non-nil) when the job has
// never run or every recent run lacks a SessionID. Limit clamping mirrors
// Recent / List.
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
	// R249-ARCH-1 (#969): clamp to the configured retention cap (s.keepCount)
	// rather than the hardcoded DefaultRunsKeepCount, mirroring List. Honours
	// an operator-raised RunsKeepCount; identical to prior behaviour when
	// retention is left at the default.
	if n > s.keepCount {
		n = s.keepCount
	}
	// Cache-warm fast path: read SessionIDs directly off the ring under
	// entry.mu without materialising a CronRunSummary slice. Mirrors the
	// before=zero branch of List → cacheGet but skips ringSnapshot's
	// per-row value copy.
	if v, ok := s.recentCache.Load(jobID); ok {
		entry := v.(*recentCacheEntry)
		entry.mu.RLock()
		if entry.warm {
			limit := n
			if limit > entry.count {
				limit = entry.count
			}
			out := make([]string, 0, limit)
			for i := 0; i < limit; i++ {
				if sid := entry.ringRead(i).SessionID; sid != "" {
					out = append(out, sid)
				}
			}
			entry.mu.RUnlock()
			return out
		}
		entry.mu.RUnlock()
	}
	// Cold path: fall back to the cached/disk Recent walk. We pay the
	// per-row copy here but cold misses are rare (warmCache lazy-fills on
	// first List/Recent), so the steady-state allocation profile is the
	// fast path above.
	rows := s.List(jobID, n, time.Time{})
	out := make([]string, 0, len(rows))
	for i := range rows {
		if sid := rows[i].SessionID; sid != "" {
			out = append(out, sid)
		}
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
// readRunNoLstat — callers that have already filtered the DirEntry by
// type. Centralising the byte-decode keeps the over-cap and unmarshal-
// error wrapping paths identical with parseRunFromFile.
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

// DeleteJob removes the entire runs/<jobID>/ subtree. Called from
// Scheduler.DeleteJobByID/DeleteJob. Idempotent: missing dir is a no-op.
// Does NOT delete ~/.claude/projects/<cwd>/<session_id>.jsonl files —
// those are user-facing claude session logs (RFC §2.3).
//
// CROSS-STORE ORDERING & CRASH RECOVERY (R242-ARCH-19 / #762):
// runs/ and cron_jobs.json are physically separate files with NO atomic
// transaction spanning both. The delete sequence in withJobByPrefix is:
//
//	(1) deleteJobLocked(j) drops the job from the in-memory map under s.mu;
//	(2) persistJobsLocked() marshals that post-delete snapshot under s.mu;
//	(3) postCleanup runs runStore.DeleteJob (this function) lock-free;
//	(4) save() lands the marshaled cron_jobs.json.
//
// So runs/<jobID>/ is removed at (3) BEFORE the job's absence is durably
// written at (4). The only crash window is between (3) and (4): runs/ is
// already gone but cron_jobs.json still lists the job. Recovery is benign
// and self-healing — on restart the job reloads with an empty history,
// re-schedules, and its first run repopulates runs/<jobID>/. The reverse
// ordering (write cron_jobs.json first, then remove runs/) was rejected
// because a crash in that window would orphan a runs/<jobID>/ subtree for a
// job no longer in cron_jobs.json, which trimAll never reclaims (it only
// trims dirs of *known* jobs) — a strictly worse leak than the transient
// empty-history above. We therefore keep "remove runs/ first" and document
// the recoverable window here rather than add a two-phase commit. R238-GO-3
// also relies on this: DeleteJob fires even when (4)'s persist later fails,
// so a persist failure does not leak runs/ on disk.
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
	// R246-GO-4: drop MkdirAll cache so a subsequent Append recreates the dir
	// (otherwise a delete + re-create-on-disk-only-by-operator scenario would
	// silently miss the mkdir).
	s.jobDirEnsured.Delete(jobID)
	s.cacheInvalidate(jobID)
	// R249-ARCH-3 (#971): reclaim the per-job *sync.Mutex too. jobLock's
	// godoc claimed the jobLocks set is "bounded by maxJobsHardCap", but —
	// unlike runningJobs which is swept on DeleteJob (R242-ARCH-15) — these
	// entries were never reclaimed, so a long-lived deployment that creates
	// and deletes thousands of jobs grows the map without limit. Deleting
	// under the held lock is safe: a concurrent caller that already loaded
	// THIS mutex still serialises on it; one that loads after the Delete
	// gets a fresh mutex, which is the same "deleted job races a concurrent
	// Append on the same ID" edge the godoc already documents (and which is
	// benign because the runs/ subtree is gone and the job left s.jobs).
	s.jobLocks.Delete(jobID)
}
