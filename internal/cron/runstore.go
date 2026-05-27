package cron

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// User-configurable defaults (DefaultRunsKeepCount / DefaultRunsKeepWindow)
// and hard schema caps (MaxRunRecordBytes) live in limits.go alongside the
// other cron-trust-boundary constants — see R247-CR-12 / R247-CR-20 (#598)
// for the rationale.

// ErrCorruptRun is returned when a run JSON file fails to parse or
// exceeds the size cap. Treated identically to "missing": list APIs
// skip the entry, GC removes it.
var ErrCorruptRun = errors.New("cron run: corrupt or oversize record")

// appendMarshalBufPool reuses bytes.Buffer + json.Encoder scratch space
// across runStore.Append calls so each Append avoids the per-call
// encodeState alloc that json.Marshal performs internally. Mirrors the
// MarshalRecord pattern in internal/eventlog/schema/record.go. Cron Append
// rate is bounded (≤ 1Hz × N jobs) but every persisted record allocates
// ~2KB of encode scratch otherwise — pooling drops that to amortised zero
// after the warmup period. R240-PERF-6 / #1043.
var appendMarshalBufPool = sync.Pool{
	New: func() any {
		return bytes.NewBuffer(make([]byte, 0, 4*1024))
	},
}

// appendMarshalPoolMaxCap drops oversized buffers from the pool so a
// one-off near-MaxRunRecordBytes record does not pin memory for the
// process lifetime. Sized at 2× MaxRunRecordBytes for headroom.
const appendMarshalPoolMaxCap = 2 * MaxRunRecordBytes

// marshalRunPooled encodes run via a pooled bytes.Buffer + json.Encoder.
// Returns a freshly-copied []byte (independent of the pooled buffer) so
// callers may retain it after the buffer is recycled. Behaviourally
// identical to json.Marshal(run) except for json.Encoder's trailing
// '\n' which is stripped to match Marshal output.
func marshalRunPooled(run *CronRun) ([]byte, error) {
	buf := appendMarshalBufPool.Get().(*bytes.Buffer)
	defer func() {
		if buf.Cap() > appendMarshalPoolMaxCap {
			return
		}
		buf.Reset()
		appendMarshalBufPool.Put(buf)
	}()
	buf.Reset()
	enc := json.NewEncoder(buf)
	// json.Marshal default — keep HTML-escape parity so on-disk bytes match
	// the legacy callers and any future Unmarshal of historical records is
	// indistinguishable from json.Marshal output.
	if err := enc.Encode(run); err != nil {
		return nil, err
	}
	body := buf.Bytes()
	if n := len(body); n > 0 && body[n-1] == '\n' {
		body = body[:n-1]
	}
	out := make([]byte, len(body))
	copy(out, body)
	return out, nil
}

// IsValidID reports whether s is a valid cron / cron-run identifier:
// a non-empty lowercase hex string of at most 64 bytes. Currently job
// and run IDs are generated as 16 hex chars; the 64-byte upper bound
// is held in reserve for a future schema bump.
//
// Accepts (returns true):
//   - "0123456789abcdef"               — canonical 16-char job/run ID
//   - "abc123"                         — short lowercase hex
//   - strings.Repeat("a", 64)          — at the 64-byte boundary
//
// Rejects (returns false):
//   - ""                               — empty
//   - "ABC123"                         — uppercase hex (lowercase only)
//   - "abc-123" / "abc.tmp" / "abc~"   — non-hex chars (rejects temp
//     files, backups, .DS_Store, etc. that may appear in runs/<jobID>/)
//   - "../etc/passwd"                  — path traversal characters
//   - strings.Repeat("a", 65)          — exceeds the 64-byte ceiling
//
// 在 store 入口（parse / list / append / detail handler）做边界校验，
// 防止 runs/<jobID>/ 下意外文件名（temp file、备份）污染 List 输出，
// 也允许 HTTP 层在请求入口直接拒绝非法 ID 而不必下沉到磁盘 IO。
// R221-FIX-P1-2 + R234-CR-10（godoc 改写为输入形态描述，不再引用
// 私有的 generateRunID / generateID）+ R249-CR-23（补 Accepts/Rejects
// 示例，明确大写 hex 一律拒绝）。
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
func (s *runStore) assertJobLockHeld(jobID string) {
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

	// R247-PERF-10 (#549): Marshal + over-cap shrink retry are pure CPU on
	// the caller-owned *run; they do not touch any runStore-shared state.
	// Hoisting them above jobLock keeps a slow JSON encode (CronRun can
	// approach maxRunBytes for chatty jobs) from serialising a concurrent
	// Append on the same jobID. WriteFileAtomic + cacheHeadPush + trim
	// stay under jobLock — those are the steps that genuinely need
	// per-job mutex serialisation. summarySrc rebinding to &shrunk on the
	// over-cap path is preserved verbatim so the cache row stays in
	// lockstep with the on-disk truncated record (#1079 / R250-GO-16).
	data, err := marshalRunPooled(run)
	if err != nil {
		slog.Warn("cron run: marshal failed", "job_id", run.JobID, "run_id", run.RunID, "err", err)
		return
	}
	summarySrc := run
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
			slog.Warn("cron run: retry marshal also exceeded cap; run record dropped",
				"job_id", run.JobID,
				"run_id", run.RunID,
				"retry_err", err2,
				"retry_bytes", retryBytes,
				"cap", s.maxRunBytes)
			return
		}
	}

	lock := s.jobLock(run.JobID)
	lock.Lock()
	defer lock.Unlock()

	dir := filepath.Join(s.root, run.JobID)
	if err := s.ensureJobDir(run.JobID, dir); err != nil {
		slog.Warn("cron run: mkdir failed", "dir", dir, "err", err)
		return
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
	// #1079: summarySrc points at the truncated copy on the over-cap retry
	// path so the cache row matches the on-disk truncated bytes.
	s.cacheHeadPush(run.JobID, summarySrc.summary())
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
// concurrent Recent / List reads. No-op on the ring when the cache
// entry is not yet warm — but we still LoadOrStore an empty placeholder
// so the next cacheGet avoids the redundant LoadOrStore + alloc on its
// own miss path. R246-GO-9 (#702): the pre-fix version returned silently
// when Load missed, leaving cacheGet to allocate the recentCacheEntry
// itself moments later.
//
// R242-GO-8 / R235-PERF-3 / R233-PERF-2: ring-buffer push in O(1).
// The pre-ring implementation did `append([]T{x}, slice...)` (later
// `append + copy + index`) which shifted up to keepCount-1 entries on
// every Append — at keepCount=200 that was 200× the per-push work the
// 1Hz cron + dashboard poll path actually needs.
func (s *runStore) cacheHeadPush(jobID string, summary CronRunSummary) {
	v, ok := s.recentCache.Load(jobID)
	if !ok {
		// Lazy-allocate the placeholder so cacheGet doesn't have to. The
		// summary is NOT seeded into the placeholder ring: warm=false stays
		// because warmCache must still read disk to pick up records that
		// predate process start. Once warmCache lands, all subsequent
		// cacheHeadPush calls observe warm=true and push into the ring.
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
	entry.ringPushHead(summary)
}

// cacheGet returns a defensive copy of up to limit newest summaries for
// jobID. Triggers a warm pass if the entry has not been hydrated yet.
// Caller must NOT hold jobLock — warmCache acquires it internally to
// populate the entry from disk.
//
// R240-GO-6 (#1039): a warm cache with count=0 is INTENTIONALLY treated as
// a hit (returns (nil, true)) — not a miss. Forcing a disk fallback on
// warm-empty would re-ReadDir on every List call for jobs that have never
// run, defeating the whole point of the cache. The "stale empty masks new
// disk row" race the original triage worried about is foreclosed by the
// jobLock contract: warmCache holds jobLock while running, and Append
// holds jobLock around its WriteFileAtomic + cacheHeadPush, so the two
// cannot interleave. After warmCache releases jobLock, any subsequent
// Append's cacheHeadPush observes warm=true and pushes into the ring,
// and the next cacheGet sees the new row. Empty caches do not stay empty
// once a run lands — they stay correct.
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
	rows, corruptCount := s.diskListNewestFirst(jobID, s.keepCount, time.Time{})
	entry.ringSeed(rows, s.keepCount)
	entry.warm = true
	if corruptCount > 0 {
		slog.Warn("cron runstore warmCache skipped corrupt files",
			"count", corruptCount, "dir", filepath.Join(s.root, jobID))
	}
}

// cacheGetBefore is the before-cutoff variant of cacheGet. It serves a
// before-filtered, newest-first slice from the cache only when the cache
// is provably exhaustive — i.e. cache.count < keepCount, meaning every
// on-disk row already lives in the ring and no entry has ever been
// trimmed off the tail. In that regime the cache holds strictly the
// same rows as a fresh disk scan, so a filter walk is correctness-
// equivalent to diskListNewestFirst at zero ReadDir+ReadFile cost.
//
// Returns ok=false when count == keepCount (the cache may have shed
// older entries via trim) — caller falls back to disk so pagination
// beyond the cache horizon still works. Cold cache paths are NOT warmed
// here: a cold-cache before-cutoff query is rare (dashboard typically
// drives warm via a no-cutoff first page), so paying the warm cost on
// the pagination path would add a ReadDir+per-file ReadFile to a query
// that is already going to disk and reading it twice — once for warm,
// once via the disk fallback. The warm path lazy-warms on the next
// no-cutoff List call. R243-PERF-5 (#810).
//
// Caller must guard before.IsZero() == false; use cacheGet for the
// no-cutoff fast path.
func (s *runStore) cacheGetBefore(jobID string, limit int, before time.Time) ([]CronRunSummary, bool) {
	v, ok := s.recentCache.Load(jobID)
	if !ok {
		return nil, false
	}
	entry := v.(*recentCacheEntry)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !entry.warm {
		return nil, false
	}
	// Exhaustive only when cache hasn't hit cap. count == keepCount
	// means trimJobLocked may have evicted older rows that match the
	// before cutoff; disk scan is the safe answer.
	if entry.count >= s.keepCount {
		return nil, false
	}
	out := make([]CronRunSummary, 0, limit)
	for i := 0; i < entry.count && len(out) < limit; i++ {
		r := entry.ringRead(i)
		// diskListNewestFirst applies StartedAt strict-less-than the
		// cutoff; mirror that here so cache and disk paths stay in
		// lockstep on the equality boundary.
		if !before.IsZero() && !r.StartedAt.Before(before) {
			continue
		}
		out = append(out, r)
	}
	return out, true
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
	rows, _ := s.diskListNewestFirst(jobID, limit, before)
	return rows
}

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
	items := make([]runDirItem, 0, len(entries))
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
		items = append(items, runDirItem{
			path:  filepath.Join(dir, name),
			runID: runID,
			mtime: info.ModTime(),
		})
	}
	slices.SortFunc(items, func(a, b runDirItem) int {
		// mtime DESC: newer first. Time.Compare (Go 1.20+) instead of
		// UnixNano so wall-clock jumps don't desync trim ↔ list ordering
		// (R236-QA-01). R235-PERF-17.
		if c := b.mtime.Compare(a.mtime); c != 0 {
			return c
		}
		// Equal-mtime tie-break by runID DESC for cross-process stability.
		// R222-GO-5.
		return cmp.Compare(b.runID, a.runID)
	})
	return items, dir, nil
}

// diskListNewestFirst is the on-disk variant of List, used by warmCache
// and as the fall-through when cache is unavailable / before-cutoff is
// set. Returns the summary list and the count of corrupt/unreadable run
// files skipped during the scan (R20260526-CR-018, #1227 — surfaces
// silent skips so warmCache can emit a single aggregate log line).
//
// R239-PERF-5 (#871): scan + sort delegated to scanSortedRunDir so this
// path stays in lockstep with trimJobLocked.
func (s *runStore) diskListNewestFirst(jobID string, limit int, before time.Time) ([]CronRunSummary, int) {
	items, dir, err := s.scanSortedRunDir(jobID)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("cron run: list readdir", "dir", dir, "err", err)
		}
		return nil, 0
	}

	out := make([]CronRunSummary, 0, limit)
	corruptCount := 0
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
			}
			continue
		}
		if !before.IsZero() && !run.StartedAt.Before(before) {
			continue
		}
		out = append(out, run.summary())
	}
	return out, corruptCount
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
// R235-SEC-5 / R242-GO-17 / R238-SEC-7 (#827): the original implementation
// did Lstat + (cond) ReadFile, which left a TOCTOU window — between the
// Lstat result observing a regular file and ReadFile opening the path,
// an attacker with write access to runs/<jobID>/ could swap the entry
// for a symlink and have ReadFile dereference a sensitive file. We close
// the window by using OpenFile with O_NOFOLLOW (kernel refuses to follow
// a final-component symlink, returning ELOOP) and Fstat'ing the resulting
// fd: the bytes we read come from exactly the inode whose mode we just
// validated as a regular file, regardless of any concurrent rename. The
// guard is the only barrier between Get() and a malicious symlink because
// Get() takes a caller-supplied runID that has not been ReadDir-filtered.
// diskListNewestFirst / trimJobLocked already skip symlinks during their
// directory scans, so they use readRunNoLstat to avoid paying for the
// redundant fd validation.
func (s *runStore) readRun(path string) (*CronRun, error) {
	// openRunFile is platform-specialised: Unix uses O_NOFOLLOW for a
	// kernel-atomic symlink refusal; Windows falls back to a Lstat-then-
	// Open two-step (best-effort, since O_NOFOLLOW is Unix-only).
	f, err := openRunFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Fstat on the fd returns metadata for the exact inode we have
	// open — no second path lookup, no race window. Reject anything
	// that's not a plain file: Open with O_NOFOLLOW already filtered
	// symlinks, but a fifo/socket/device with the right name would
	// still get past Open and only Fstat catches it.
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: not a regular file", ErrCorruptRun)
	}
	return s.parseRunFromFile(f, fi)
}

// readRunNoLstat is the loop-friendly variant of readRun for callers that
// have already filtered the entry through DirEntry.Type() (rejecting symlinks
// + non-regular modes during the directory scan). It skips the redundant
// Lstat syscall, halving syscall count for large directory listings —
// R245-PERF-9 (cluster: R243-PERF-11).
//
// SAFETY: must NOT be used as the entry-point for a constructed path (e.g.
// Get()'s direct path lookup). Get arrives with a caller-supplied runID
// that has not been ReadDir-filtered, so the Lstat guard in readRun is the
// only barrier against `runs/<jobID>/<runID>.json` being a symlink to
// /etc/passwd. diskListNewestFirst is the sole caller because its scan loop
// already drops symlinks at the e.Type()&fs.ModeSymlink check.
func (s *runStore) readRunNoLstat(path string) (*CronRun, error) {
	return s.parseRunBytes(path)
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

// parseRunFromFile reads the open fd's contents (bounded by maxRunBytes+1
// so we can detect oversize without slurping arbitrary bytes) and decodes
// the JSON. Used by readRun where the fd is the TOCTOU-safe handle. fi is
// the Fstat result already validated as a regular file, used to size-hint
// the buffer.
func (s *runStore) parseRunFromFile(f *os.File, fi os.FileInfo) (*CronRun, error) {
	// io.ReadAll grows incrementally; preallocate when fi.Size() is a
	// reasonable hint. The cap+1 read pattern is irrelevant here because
	// decodeRunBytes enforces the cap explicitly on the returned slice
	// length, so even a regular file that grew between Stat and Read
	// gets rejected by the size check.
	size := fi.Size()
	if size < 0 || size > s.maxRunBytes {
		// Stat already says we're over cap — short-circuit before any
		// ReadAll alloc. Match parseRunBytes's wrap exactly so callers
		// can't tell the readRun vs readRunNoLstat path apart by error
		// shape.
		return nil, fmt.Errorf("%w: %d bytes > %d cap", ErrCorruptRun, size, s.maxRunBytes)
	}
	buf := make([]byte, 0, size)
	data, err := readAllInto(f, buf)
	if err != nil {
		return nil, err
	}
	return decodeRunBytes(data, s.maxRunBytes)
}

// readAllInto reads f to EOF, appending into the supplied prefix-allocated
// buffer. Mirrors io.ReadAll's loop but lets the caller pre-size based on
// Fstat to avoid repeated re-grows on the typical ~2KB run record.
func readAllInto(f *os.File, buf []byte) ([]byte, error) {
	for {
		if len(buf) == cap(buf) {
			buf = append(buf, 0)[:len(buf)]
		}
		n, err := f.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buf, nil
			}
			return buf, err
		}
	}
}

// decodeRunBytes enforces the size cap and json.Unmarshal step shared by
// both file-based and bytes-based read paths. Extracted from parseRunBytes
// so parseRunFromFile (the TOCTOU-safe path) can reuse the wrapping shape
// without an extra ReadFile.
func decodeRunBytes(data []byte, maxRunBytes int64) (*CronRun, error) {
	if int64(len(data)) > maxRunBytes {
		return nil, fmt.Errorf("%w: %d bytes > %d cap", ErrCorruptRun, len(data), maxRunBytes)
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
	// items sorted newest first by scanSortedRunDir, so rank checking
	// below is index-based. Sort policy / tie-break rationale lives on
	// scanSortedRunDir's godoc; trim and list paths must observe the same
	// total order or the cap cutoff (i < keepCount) and the list cutoff
	// (StartedAt < before) disagree about which equal-mtime record to
	// drop. R235-GO-7 / R236-QA-01.
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
func (s *runStore) trimSkipFromCache(jobID string, now time.Time) bool {
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
	for _, e := range entries {
		// 在每个 job 入口前检查 ctx；scheduler.Stop 触发 stopCancel 后
		// 当前 job 完成即退出循环，避免 Stop 等到 gcWaitBudget 超时。
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
