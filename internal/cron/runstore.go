package cron

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
}

// recentCacheEntry is the cached newest-first snapshot for one job.
//
// R242-GO-8 / R235-PERF-3 / R233-PERF-2: storage is a fixed-capacity ring
// buffer (cap = runStore.keepCount, typically 200). cacheHeadPush is the
// hot path — every Append calls it, so pre-historical implementations
// that did `append + copy` shifted up to keepCount-1 entries per push
// (O(N) per Append). The ring lets us land each push in O(1) by
// rotating `head` backwards instead of moving data.
//
// Logical view: newest-first slice of length `count`, where index 0 is
// the newest entry. Physically: `ring[head]` is the newest, `ring[(head
// + count - 1) % cap(ring)]` is the oldest. ringRead / ringSnapshot
// translate logical → physical for all consumers.
type recentCacheEntry struct {
	mu sync.Mutex
	// ring is the fixed-capacity backing array. cap(ring) == runStore.keepCount
	// after the first warm pass; nil before warm.
	ring []CronRunSummary
	// head is the ring index of the newest entry. Undefined when count == 0.
	head int
	// count is the populated length (0 ≤ count ≤ cap(ring)).
	count int
	warm  bool // false until first warm() pass; List/Recent will lazy-warm
	// appendsSinceTrim counts Append calls since the last full trimJobLocked
	// pass. Used by skipAppendTrim to batch ReadDir-driven trims when the
	// cache shows we're well under keepCount. Reset to 0 by Append after
	// calling trimJobLocked. R232-PERF-8.
	appendsSinceTrim int
}

// ringRead returns the i-th newest entry (0 = newest). Caller holds entry.mu
// and must ensure 0 ≤ i < entry.count.
func (e *recentCacheEntry) ringRead(i int) CronRunSummary {
	// R247-GO-4: defensive against cap(ring)==0 with count>0 — same self-heal
	// philosophy as cacheHeadPush's `cap(entry.ring) != s.keepCount` reseed.
	// Avoids integer divide-by-zero panic on a regression path that bypasses
	// ringSeed (e.g. an unwarmed entry mutated by future code). [BREAKING-LOCAL]
	if cap(e.ring) == 0 {
		return CronRunSummary{}
	}
	return e.ring[(e.head+i)%cap(e.ring)]
}

// ringSnapshot returns a fresh newest-first slice of up to limit entries.
// Caller holds entry.mu. limit ≤ 0 or limit > count returns count entries.
func (e *recentCacheEntry) ringSnapshot(limit int) []CronRunSummary {
	// R247-GO-4: see ringRead — guard cap=0 + count>0 regression and the
	// degenerate count==0 fast path (no allocation needed). [BREAKING-LOCAL]
	if cap(e.ring) == 0 || e.count == 0 {
		return nil
	}
	if limit <= 0 || limit > e.count {
		limit = e.count
	}
	out := make([]CronRunSummary, limit)
	c := cap(e.ring)
	// Two contiguous segments: head..min(head+limit, c) and 0..wrap.
	first := limit
	if e.head+first > c {
		first = c - e.head
	}
	copy(out, e.ring[e.head:e.head+first])
	if first < limit {
		copy(out[first:], e.ring[:limit-first])
	}
	return out
}

// ringPushHead inserts summary at the newest end in O(1). Caller holds
// entry.mu and entry.ring is allocated (cap > 0).
func (e *recentCacheEntry) ringPushHead(summary CronRunSummary) {
	c := cap(e.ring)
	// Move head one slot backwards, wrapping around. After this, the
	// freshly written summary is the newest entry.
	e.head = (e.head - 1 + c) % c
	if e.count == 0 {
		// First push into an empty ring: ensure ring length covers head.
		// We keep len(ring) == cap(ring) so plain index assignment works
		// regardless of count.
		e.ring = e.ring[:c]
	}
	e.ring[e.head] = summary
	if e.count < c {
		e.count++
	}
}

// ringSeed populates the ring from a newest-first source slice. Caller
// holds entry.mu. Used by warmCache and cacheTrimAfterDisk to install a
// fresh snapshot. cap is set to keepCount so future pushes never realloc.
func (e *recentCacheEntry) ringSeed(rows []CronRunSummary, keepCount int) {
	if cap(e.ring) != keepCount {
		e.ring = make([]CronRunSummary, keepCount)
	} else {
		e.ring = e.ring[:keepCount]
		// Zero out trailing slots so old entries beyond count don't pin
		// strings / sub-slices (avoid leaking RAM through a smaller seed).
		for i := len(rows); i < keepCount; i++ {
			e.ring[i] = CronRunSummary{}
		}
	}
	n := len(rows)
	if n > keepCount {
		n = keepCount
	}
	copy(e.ring[:n], rows[:n])
	e.head = 0
	e.count = n
}

const (
	// DefaultRunsKeepCount caps per-job history at this many entries.
	// 200 is the user-confirmed upper bound (cron-run-history.md §4.3 +
	// chat conversation 2026-05-17).
	DefaultRunsKeepCount = 200

	// DefaultRunsKeepWindow ages out runs older than this even when the
	// per-job count is below the cap. AND-with-OR semantics: a run is
	// kept only when (count_rank ≤ keepCount) AND (age ≤ keepWindow);
	// either condition false → trim.
	DefaultRunsKeepWindow = 30 * 24 * time.Hour

	// MaxRunRecordBytes caps a single CronRun JSON payload. The 4K
	// rune cap on Result + 512-rune cap on ErrorMsg + 8K Prompt + ~512
	// metadata add up to ~13 KiB worst case; 32 KiB leaves headroom.
	// Reading a file larger than this returns ErrCorruptRun.
	MaxRunRecordBytes = 32 * 1024
)

// ErrCorruptRun is returned when a run JSON file fails to parse or
// exceeds the size cap. Treated identically to "missing": list APIs
// skip the entry, GC removes it.
var ErrCorruptRun = errors.New("cron run: corrupt or oversize record")

// IsValidID reports whether s is a valid cron / cron-run identifier:
// a non-empty lowercase hex string of at most 64 bytes. Currently job
// and run IDs are generated as 16 hex chars; the 64-byte upper bound
// is held in reserve for a future schema bump.
//
// 在 store 入口（parse / list / append / detail handler）做边界校验，
// 防止 runs/<jobID>/ 下意外文件名（temp file、备份）污染 List 输出，
// 也允许 HTTP 层在请求入口直接拒绝非法 ID 而不必下沉到磁盘 IO。
// R221-FIX-P1-2 + R234-CR-10（godoc 改写为输入形态描述，不再引用
// 私有的 generateRunID / generateID）。
func IsValidID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// newRunStore constructs a runStore rooted at <storePath dir>/runs.
// storePath="" disables the store (List returns empty, Append no-ops);
// callers can pass a Scheduler in tests without wiring up a tempdir.
func newRunStore(storePath string, keepCount int, keepWindow time.Duration) *runStore {
	if storePath == "" {
		return &runStore{disabled: true}
	}
	if keepCount <= 0 {
		keepCount = DefaultRunsKeepCount
	}
	if keepWindow <= 0 {
		keepWindow = DefaultRunsKeepWindow
	}
	root := filepath.Join(filepath.Dir(storePath), "runs")
	// R234-SEC-4: 主动创建 runs/ 根目录并设 0o700。原先只在 Append 时
	// 创建 runs/<jobID>/ 子目录用 0o700，而 runs/ 自身继承父目录权限
	// （通常 0o755），同机器其他 OS 用户可枚举 jobID 列表，泄露 cron
	// 任务存在与数量。失败仅记录 Warn — 后续 Append 仍会在子目录路径
	// 上 MkdirAll，不影响功能。
	if err := os.MkdirAll(root, 0o700); err != nil {
		slog.Warn("cron run: mkdir root failed", "root", root, "err", err)
	}
	return &runStore{
		root:         root,
		keepCount:    keepCount,
		keepWindow:   keepWindow,
		maxRunBytes:  MaxRunRecordBytes,
		enableTrimGC: true,
	}
}

// jobLock returns a *sync.Mutex unique to jobID. Lazily allocated and
// never reclaimed (entries are bounded by maxJobsHardCap; a deleted job
// races a concurrent Append on the very same ID is the same edge handled
// by the runningJobs sync.Map).
func (s *runStore) jobLock(jobID string) *sync.Mutex {
	if v, ok := s.jobLocks.Load(jobID); ok {
		return v.(*sync.Mutex)
	}
	m := &sync.Mutex{}
	actual, _ := s.jobLocks.LoadOrStore(jobID, m)
	return actual.(*sync.Mutex)
}

// assertJobLockHeld panics when jobLock(jobID) is currently free, which
// — outside concurrent tests — is the unambiguous signature of a caller
// that violated the *Locked-suffix contract (forgot to acquire). Use
// from helpers whose godoc says "caller must hold jobLock".
//
// The check is best-effort: TryLock+Unlock is cheap (uncontended fast
// path) and the panic message includes the jobID so failures point
// straight at the offending caller. False negatives — another goroutine
// holds the lock, our caller doesn't, TryLock fails so we miss the bug
// — are accepted in exchange for catching the dominant "single-flight
// test caller forgot to lock" failure mode reliably. R236-GO-03.
func (s *runStore) assertJobLockHeld(jobID string) {
	lock := s.jobLock(jobID)
	if lock.TryLock() {
		lock.Unlock()
		panic("cron runstore: jobLock(" + jobID + ") not held by caller; *Locked-suffix contract violated")
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
	if _, ok := s.jobDirEnsured.Load(jobID); ok {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		// 不写入 cache：让下次 Append 重试。
		return err
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
	lock := s.jobLock(run.JobID)
	lock.Lock()
	defer lock.Unlock()

	dir := filepath.Join(s.root, run.JobID)
	if err := s.ensureJobDir(run.JobID, dir); err != nil {
		slog.Warn("cron run: mkdir failed", "dir", dir, "err", err)
		return
	}
	data, err := json.Marshal(run)
	if err != nil {
		slog.Warn("cron run: marshal failed", "job_id", run.JobID, "run_id", run.RunID, "err", err)
		return
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
		if data2, err2 := json.Marshal(&shrunk); err2 == nil && int64(len(data2)) <= s.maxRunBytes {
			data = data2
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
			slog.Warn("cron run: retry marshal also exceeded cap; run record dropped",
				"job_id", run.JobID,
				"run_id", run.RunID,
				"retry_err", err2,
				"retry_bytes", retryBytes,
				"cap", s.maxRunBytes)
			return
		}
	}
	path := filepath.Join(dir, run.RunID+".json")
	if err := osutil.WriteFileAtomic(path, data, 0o600); err != nil {
		slog.Warn("cron run: write failed", "path", path, "err", err, "disk_full", osutil.IsDiskFull(err))
		return
	}
	// Push to recentCache head while still under jobLock so concurrent
	// Append + Recent see consistent newest-first order. Cache may not
	// yet be warm for this jobID — that's fine: cacheHeadPush is a no-op
	// then, and the next Recent call will lazy-warm via warmCache.
	s.cacheHeadPush(run.JobID, run.summary())
	if s.enableTrimGC && !s.skipAppendTrim(run.JobID) {
		s.trimJobLocked(run.JobID, time.Now())
	}
}

// skipAppendTrim returns true when the cache for jobID indicates the on-disk
// run count is comfortably below keepCount and the keepWindow age policy
// won't have anything to evict yet (oldest cached row newer than cutoff).
// In that case Append's per-call trimJobLocked → ReadDir is pure overhead;
// running it once every appendTrimBatch Appends is enough to clean up
// background drift without paying ReadDir on every call. R232-PERF-8.
//
// Falls back to "do not skip" when the cache is cold or the safety margins
// don't hold — a missed trim is bounded by appendTrimBatch and the next
// trimAll cold-pass, so over-keeping a few entries is acceptable; missing a
// trim entirely is not.
//
// CALLER CONTRACT: caller MUST hold jobLock(jobID). The function bumps
// entry.appendsSinceTrim and resets it on the "do trim" branches; without
// jobLock serialisation two parallel Appends can both hit the
// >= appendTrimBatch boundary, mis-coalesce trim cadence, or — combined
// with cacheHeadPush which is also jobLock-serialised — race trimJobLocked
// against a fresh Append's WriteFileAtomic. Today the sole caller is
// Append (runstore.go:252) which acquires jobLock at line 213; any future
// helper must do the same. R239-GO-5.
func (s *runStore) skipAppendTrim(jobID string) bool {
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
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !entry.warm {
		return false
	}
	// Force a trim every appendTrimBatch Appends so window-based eviction
	// still happens for jobs that never approach keepCount.
	entry.appendsSinceTrim++
	if entry.appendsSinceTrim >= appendTrimBatch {
		entry.appendsSinceTrim = 0
		return false
	}
	// Plenty of headroom under count cap?  Cache reflects the on-disk
	// newest-first ring (capped to keepCount), so entry.count is a safe
	// upper bound on disk rows that survived the last trim.
	if entry.count+appendTrimBatch >= s.keepCount {
		entry.appendsSinceTrim = 0
		return false
	}
	// Oldest cached row still inside keepWindow?  Use EndedAt to mirror
	// trimJobLocked's mtime-based cutoff (cacheTrimAfterDisk also approximates
	// mtime via EndedAt — keep these two paths consistent).
	if entry.count > 0 {
		oldest := entry.ringRead(entry.count - 1)
		ts := oldest.EndedAt
		if ts.IsZero() {
			ts = oldest.StartedAt
		}
		cutoff := time.Now().Add(-s.keepWindow)
		if !ts.After(cutoff) {
			entry.appendsSinceTrim = 0
			return false
		}
	}
	return true
}

// appendTrimBatch is the maximum number of Append calls we'll let pass
// without running trimJobLocked when skipAppendTrim's safety conditions
// hold. Picked low enough that even a runaway 1 Hz job sees a trim every
// 10 s.
const appendTrimBatch = 10

// cacheHeadPush prepends summary to the recentCache for jobID. The
// caller must hold jobLock(jobID) so the push is serialised against
// concurrent Recent / List reads. No-op when the cache entry is not yet
// warm (List/Recent will populate from disk on first miss).
//
// R242-GO-8 / R235-PERF-3 / R233-PERF-2: ring-buffer push in O(1).
// The pre-ring implementation did `append([]T{x}, slice...)` (later
// `append + copy + index`) which shifted up to keepCount-1 entries on
// every Append — at keepCount=200 that was 200× the per-push work the
// 1Hz cron + dashboard poll path actually needs.
func (s *runStore) cacheHeadPush(jobID string, summary CronRunSummary) {
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
	// Defensive: a warm cache must always own a cap=keepCount ring (warmCache
	// guarantees this via ringSeed). Re-allocate if a future caller bypasses
	// ringSeed; cheap and avoids index-out-of-range under unexpected input.
	if cap(entry.ring) != s.keepCount {
		entry.ringSeed(nil, s.keepCount)
	}
	entry.ringPushHead(summary)
}

// cacheGet returns a defensive copy of up to limit newest summaries for
// jobID. Triggers a warm pass if the entry has not been hydrated yet.
// Caller must NOT hold jobLock — warmCache acquires it internally to
// populate the entry from disk.
func (s *runStore) cacheGet(jobID string, limit int) ([]CronRunSummary, bool) {
	v, ok := s.recentCache.Load(jobID)
	if !ok {
		// Lazy-allocate the entry; warmCache will populate it.
		entry := &recentCacheEntry{}
		actual, _ := s.recentCache.LoadOrStore(jobID, entry)
		v = actual
	}
	entry := v.(*recentCacheEntry)
	entry.mu.Lock()
	if entry.warm {
		out := entry.ringSnapshot(limit)
		entry.mu.Unlock()
		return out, true
	}
	entry.mu.Unlock()

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
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !entry.warm {
		return nil, false
	}
	return entry.ringSnapshot(limit), true
}

// warmCache populates the recentCache for jobID by reading the on-disk
// runs/<jobID>/ directory and parsing each .json file. Holds the per-job
// disk lock so a concurrent Append can't race the warm pass.
func (s *runStore) warmCache(jobID string) {
	lock := s.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()

	v, _ := s.recentCache.LoadOrStore(jobID, &recentCacheEntry{})
	entry := v.(*recentCacheEntry)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.warm {
		return // another goroutine warmed it during our wait
	}
	rows := s.diskListNewestFirst(jobID, s.keepCount, time.Time{})
	entry.ringSeed(rows, s.keepCount)
	entry.warm = true
}

// cacheInvalidate forgets the cache entry for jobID. Used by DeleteJob.
func (s *runStore) cacheInvalidate(jobID string) {
	s.recentCache.Delete(jobID)
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
	if limit > DefaultRunsKeepCount {
		limit = DefaultRunsKeepCount
	}

	// Cache fast-path: when before is zero (most common — Recent and the
	// first paginated page) and the entry is warm, return without IO.
	// before-cutoff queries always go to disk because the cache only
	// holds keepCount newest; pagination beyond that needs the filtered
	// scan. R220-PERF-1.
	if before.IsZero() {
		if cached, ok := s.cacheGet(jobID, limit); ok {
			return cached
		}
	}
	return s.diskListNewestFirst(jobID, limit, before)
}

// diskListNewestFirst is the on-disk variant of List, used by warmCache
// and as the fall-through when cache is unavailable / before-cutoff is
// set. Same return contract as List.
func (s *runStore) diskListNewestFirst(jobID string, limit int, before time.Time) []CronRunSummary {
	dir := filepath.Join(s.root, jobID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("cron run: list readdir", "dir", dir, "err", err)
		}
		return nil
	}
	type item struct {
		runID string
		mtime time.Time
	}
	items := make([]item, 0, len(entries))
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
		items = append(items, item{runID: runID, mtime: info.ModTime()})
	}
	// Sort by mtime desc; mtime tracks WriteFileAtomic landing. StartedAt
	// inside the JSON is the real source of truth, but ReadDir + Stat is
	// cheap; reading every JSON to sort by StartedAt would inflate list
	// latency from O(N) stat to O(N) parse.
	//
	// R222-GO-5: when the underlying FS has low timestamp precision (FAT32 ≈
	// 2 s, ext3 ≈ 1 s, tmpfs occasionally collapses concurrent atomic writes
	// to the same nanosecond), two runs that complete in the same tick get
	// identical mtimes and ReadDir's iteration order becomes load-bearing —
	// flipping ordering on rerun and letting pagination cutoff (StartedAt <
	// before) silently skip a record. Use runID as a deterministic secondary
	// key: it's 16-char random hex, so the tie-breaker is stable across
	// processes and re-reads even though it carries no time signal of its own.
	slices.SortFunc(items, func(a, b item) int {
		// mtime DESC: newer first. time.Time.Compare (Go 1.20+) returns
		// -1/0/+1 in a single call vs. the prior Equal+After pair.
		// R235-PERF-17.
		if c := b.mtime.Compare(a.mtime); c != 0 {
			return c
		}
		// Same mtime: runID DESC tie-break. R222-GO-5.
		return cmp.Compare(b.runID, a.runID)
	})

	out := make([]CronRunSummary, 0, limit)
	for _, it := range items {
		if len(out) >= limit {
			break
		}
		path := filepath.Join(dir, it.runID+".json")
		run, err := s.readRun(path)
		if err != nil {
			continue
		}
		if !before.IsZero() && !run.StartedAt.Before(before) {
			continue
		}
		out = append(out, run.summary())
	}
	return out
}

// Recent returns the N most recent CronRunSummary entries for jobID
// (newest first). Convenience wrapper over List with limit=n, before=zero —
// hits cache on warm path. R220-PERF-1.
func (s *runStore) Recent(jobID string, n int) []CronRunSummary {
	return s.List(jobID, n, time.Time{})
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

// readRun parses a single run file. Returns ErrCorruptRun on parse
// failure or oversize; fs.ErrNotExist propagates unchanged.
//
// R235-SEC-5 / R242-GO-17: Lstat is intentionally used here (not Stat).
// Stat would follow symlinks, which an attacker with write access to
// runs/<jobID>/ could exploit by replacing a legitimate .json with a
// symlink to /etc/passwd or another sensitive file. Lstat reports the
// link itself, so the IsRegular() check below rejects the symlink before
// ReadFile dereferences it. diskListNewestFirst / trimJobLocked already
// skip symlinks during directory scans, but Get() arrives here directly
// with a constructed path so this guard is the only barrier.
func (s *runStore) readRun(path string) (*CronRun, error) {
	li, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !li.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: not a regular file", ErrCorruptRun)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > s.maxRunBytes {
		return nil, fmt.Errorf("%w: %d bytes > %d cap", ErrCorruptRun, len(data), s.maxRunBytes)
	}
	var run CronRun
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorruptRun, err)
	}
	return &run, nil
}

// DeleteJob removes the entire runs/<jobID>/ subtree. Called from
// Scheduler.DeleteJobByID/DeleteJob. Idempotent: missing dir is a no-op.
// Does NOT delete ~/.claude/projects/<cwd>/<session_id>.jsonl files —
// those are user-facing claude session logs (RFC §2.3).
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
}

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
// Policy: keep ALL runs satisfying BOTH (rank ≤ keepCount) AND
// (age ≤ keepWindow). Either condition violated → delete. AND-vs-OR
// is the user-confirmed choice in the RFC chat (§4.3): high-frequency
// jobs get capped by count; low-frequency jobs by window.
func (s *runStore) trimJobLocked(jobID string, now time.Time) {
	s.assertJobLockHeld(jobID)
	dir := filepath.Join(s.root, jobID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type item struct {
		path  string
		runID string
		mtime time.Time
	}
	items := make([]item, 0, len(entries))
	cutoff := now.Add(-s.keepWindow)
	anyExpired := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// 跳过 symlink，与 diskListNewestFirst 对齐（path traversal 防御）。
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
		mtime := info.ModTime()
		if !mtime.After(cutoff) {
			anyExpired = true
		}
		items = append(items, item{
			path:  filepath.Join(dir, name),
			runID: runID,
			mtime: mtime,
		})
	}
	// Fast path: under cap AND nothing expired → no sort, no remove. The
	// common case for healthy 5-min cron jobs that ride well under the
	// 200-entry cap. R220-PERF-3.
	if len(items) <= s.keepCount && !anyExpired {
		return
	}
	if len(items) == 0 {
		return
	}
	// Sort newest first so rank checking is index-based. Same total order as
	// diskListNewestFirst: mtime DESC, then runID DESC for the equal-mtime
	// tiebreak — without the runID secondary key the trim cutoff
	// (i < s.keepCount) and the list cutoff would disagree about which
	// equal-mtime record to drop, leaving a window where a record visible
	// in the list could be removed by trim. R235-GO-7.
	slices.SortFunc(items, func(a, b item) int {
		// R236-QA-01: use time.Time.Compare to mirror diskListNewestFirst
		// exactly. UnixNano() can disagree with Time.Compare across wall
		// clock jumps / monotonic-clock resets (e.g. ntp step) and would
		// break the trim-cutoff / list-cutoff equality invariant noted
		// above, leaving a record visible in the list that trim deletes
		// (or vice versa). Time.Compare is the canonical total order.
		if c := b.mtime.Compare(a.mtime); c != 0 {
			return c
		}
		return cmp.Compare(b.runID, a.runID)
	})
	for i, it := range items {
		// Both conditions must hold to keep.
		keep := i < s.keepCount && it.mtime.After(cutoff)
		if keep {
			continue
		}
		if err := os.Remove(it.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("cron run: trim remove failed", "path", it.path, "err", err)
		}
	}
	// Cache may now point to deleted entries; reconcile by trimming the
	// cache slice to the same (count + window) policy. We hold jobLock so
	// concurrent Append's cacheHeadPush can't race.
	s.cacheTrimAfterDisk(jobID, cutoff)
}

// cacheTrimAfterDisk reconciles the recentCache for jobID after on-disk
// trimJobLocked removed expired / over-cap entries. Called by trimJobLocked
// only — caller holds jobLock(jobID).
func (s *runStore) cacheTrimAfterDisk(jobID string, cutoff time.Time) {
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
	survive := 0
	for i := 0; i < limit; i++ {
		r := entry.ringRead(i)
		ts := r.EndedAt
		if ts.IsZero() {
			ts = r.StartedAt
		}
		if ts.Before(cutoff) {
			break
		}
		survive++
	}
	// Zero out the dropped slots to release any retained string fields.
	c := cap(entry.ring)
	if c > 0 {
		for i := survive; i < entry.count; i++ {
			entry.ring[(entry.head+i)%c] = CronRunSummary{}
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
	if s == nil || s.disabled {
		return
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
	for _, e := range entries {
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
		s.trimJobUnderLock(jobID, now)
	}
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
