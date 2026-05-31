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
	"sort"
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

// ringCapZeroWarnOnce ensures the cap=0 self-heal branch in ringRead /
// ringSnapshot logs exactly once per process. R249-ARCH-13 (#979): the
// defensive cap=0 guard previously returned silently, so a future regression
// that mutated count>0 while leaving cap(ring)==0 (bypassing ringSeed) would
// surface only as mysteriously-empty dashboard lists with no log trace. A
// single warn points the next reader straight at the contract violation
// without spamming the log on a hot read path.
var ringCapZeroWarnOnce sync.Once

func warnRingCapZero(site string) {
	ringCapZeroWarnOnce.Do(func() {
		slog.Warn("cron runstore: recentCache ring cap=0 on read; self-healing to empty (ringSeed bypass regression?)",
			"site", site)
	})
}

// ringRead returns the i-th newest entry (0 = newest). Caller holds entry.mu
// and must ensure 0 ≤ i < entry.count.
func (e *recentCacheEntry) ringRead(i int) CronRunSummary {
	// R247-GO-4: defensive against cap(ring)==0 with count>0 — same self-heal
	// philosophy as cacheHeadPush's `cap(entry.ring) != s.keepCount` reseed.
	// Avoids integer divide-by-zero panic on a regression path that bypasses
	// ringSeed (e.g. an unwarmed entry mutated by future code). [BREAKING-LOCAL]
	if cap(e.ring) == 0 {
		// R249-ARCH-13 (#979): warn once so the silent self-heal is auditable.
		warnRingCapZero("ringRead")
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
		// R249-ARCH-13 (#979): warn once when cap=0 *despite* a populated
		// count — that is the bypass regression. The count==0 case is the
		// benign empty-cache fast path and stays silent.
		if cap(e.ring) == 0 && e.count > 0 {
			warnRingCapZero("ringSnapshot")
		}
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
		// directly. We still emit the same warn line so existing log-based
		// alerting on "payload exceeds size cap" stays calibrated.
		slog.Warn("cron run: payload exceeds size cap; truncating result/prompt and retrying",
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
//
// R245-PERF-14 (#863) proposed converting appendsSinceTrim to atomic.Int32
// to skip entry.mu on the no-race fast path. Won't-fix: the threshold-
// gate is the cheapest of the four checks below — entry.warm,
// entry.count, oldest-row.EndedAt all require entry.mu since they read
// the ring state, and ringRead derives from cap(ring) which is mutated
// by ringSeed under the same mutex. Atomicising just the counter would
// not let us release entry.mu, so the lock-elision premise of the
// proposal does not hold without a much larger redesign (split the
// counter from the ring state, double-buffer the ring, etc.). The
// existing jobLock + entry.mu pair runs in <100ns on the warm path
// already; the remaining cost is dominated by ringRead's modulo, not
// the lock itself.
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
	// R20260527-PERF-24 (#1295): perform the cache-headroom checks BEFORE
	// the appendTrimBatch boundary. The prior order force-returned false on
	// the boundary regardless of cap/window state, walking the runs/<jobID>/
	// ReadDir+Stat tree every 10 Appends even when the cache could prove no
	// candidate exists (steady-state: 1run/min × 50 jobs × 30 days = 14400
	// wasted ReadDirs/day). The boundary is now only honoured when cap or
	// window state is unprovable from cache alone.
	entry.appendsSinceTrim++
	// Plenty of headroom under count cap?  Cache reflects the on-disk
	// newest-first ring (capped to keepCount), so entry.count is a safe
	// upper bound on disk rows that survived the last trim.
	capSafe := entry.count+appendTrimBatch < s.keepCount
	// Oldest cached row still inside keepWindow?  Use EndedAt to mirror
	// trimJobLocked's mtime-based cutoff (cacheTrimAfterDisk also approximates
	// mtime via EndedAt — keep these two paths consistent).
	windowSafe := true
	if entry.count > 0 {
		oldest := entry.ringRead(entry.count - 1)
		ts := oldest.EndedAt
		if ts.IsZero() {
			ts = oldest.StartedAt
		}
		cutoff := time.Now().Add(-s.keepWindow)
		if !ts.After(cutoff) {
			windowSafe = false
		}
	}
	if capSafe && windowSafe {
		// Both cache-state proofs hold — nothing for trimJobLocked to do
		// even on the appendTrimBatch boundary. Reset the counter so we
		// don't accumulate drift toward an inevitable forced scan that
		// would still find no work.
		entry.appendsSinceTrim = 0
		return true
	}
	// One of the two cache proofs failed — there may be on-disk work for
	// trimJobLocked. Run it now (resetting the counter); the appendTrimBatch
	// boundary is irrelevant once we've already decided to scan.
	entry.appendsSinceTrim = 0
	return false
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
// R242-GO-8 / R235-PERF-3 / R233-PERF-2 (#556): ring-buffer push in O(1).
// The pre-ring implementation did `append([]T{x}, slice...)` (later
// `append + copy + index`) which shifted up to keepCount-1 entries on
// every Append — at keepCount=200 that was 200× the per-push work the
// 1Hz cron + dashboard poll path actually needs. ringPushHead below is
// the O(1) implementation that landed via R243-PERF-4; #556 was the
// repeat finding before the cluster was wired up.
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
	// R20260527122801-PERF-4 (#1335): with WriteFileAtomic now hoisted out
	// of jobLock (so concurrent Appends do not serialise on the slow
	// fsync+rename), warmCache and Append's cacheHeadPush can interleave
	// such that warmCache reads the freshly-renamed file(s) from disk and
	// seeds them into the ring BEFORE the matching cacheHeadPush re-acquires
	// the lock to push. Without dedup, that interleaving would land the
	// same RunID twice in the ring. We scan the ring for an existing
	// matching RunID — head-only dedup is insufficient because warmCache
	// can seed multiple concurrently-written rows ahead of any of their
	// late-arriving pushes (e.g. ring [Y,X], then X's late push would
	// otherwise dup since head==Y). Cost is O(count) but the interleave
	// is rare and the ring is small (default keepCount=200); the dedup
	// fires only on the contended path, so amortised cost stays flat.
	for i := 0; i < entry.count; i++ {
		if entry.ringRead(i).RunID == summary.RunID {
			return
		}
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
// disk row" race is foreclosed by a two-part contract:
//
//  1. warmCache holds jobLock while it ReadDirs and seeds the ring; Append
//     also holds jobLock around its cacheHeadPush. Neither side can read
//     a half-installed ring.
//  2. R20260527122801-PERF-4 (#1335) hoisted Append's WriteFileAtomic OUT
//     of jobLock so a concurrent warmCache CAN now ReadDir a fresh file
//     before the matching cacheHeadPush runs. cacheHeadPush dedups by
//     RunID against the ring head so the warmCache-then-push interleave
//     does not insert a duplicate. The "fresh disk row" still becomes
//     visible — either via warmCache's seed OR via cacheHeadPush — so
//     readers never miss it. Empty caches do not stay empty once a run
//     lands; they stay correct.
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
	// R247-GO-6 (#483): re-Load() after warmCache so a concurrent
	// cacheInvalidate (DeleteJob path) that races between our initial
	// LoadOrStore and warmCache's own LoadOrStore cannot leave us
	// reading the stale `entry` reference whose `warm=false` will never
	// be flipped — warmCache populated a DIFFERENT entry under the same
	// jobID. Without this re-Load the result was a silent permanent
	// (nil, false) miss until the next Append re-seeded the cache.
	if v2, ok := s.recentCache.Load(jobID); ok {
		entry = v2.(*recentCacheEntry)
	}
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
//
// Post-condition (R241-CR-6 / #486): on return, the cache entry's
// warm flag is true REGARDLESS of disk outcome — diskListNewestFirst
// folds ReadDir failures into a nil rows + 0 corruptCount return, and
// ringSeed installs an empty ring; warm=true is set unconditionally
// before the inner Unlock. This intentionally caches the absence of
// runs (or a transient disk error) for the jobLock+entry.mu window so
// a 1Hz dashboard poller does not re-ReadDir the same failing
// directory on every call. A subsequent Append always invalidates +
// re-warms via cacheHeadPush + warmCacheLocked, so a transient ENOENT
// during process startup self-heals on the first persisted run. The
// "post-warm `if !entry.warm { return nil, false }`" guard in cacheGet
// is therefore a defensive belt for a future warmCache change rather
// than a real disk-error fallback path.
//
// R236-PERF-09 (#527, partial): the corrupt-file slog.Warn was hoisted
// past lock release so a slow stderr / structured-log shipper can't
// extend the jobLock + entry.mu window that blocks concurrent Append
// and cacheGet. The slog cost is small in steady state but unbounded
// when the operator ships logs over a slow sink — keeping observability
// out of the lock window is cheaper than auditing every log handler.
func (s *runStore) warmCache(jobID string) {
	corruptCount := s.warmCacheLocked(jobID)
	if corruptCount > 0 {
		slog.Warn("cron runstore warmCache skipped corrupt files",
			"count", corruptCount, "dir", filepath.Join(s.root, jobID))
	}
}

// warmCacheLocked is the inner critical section of warmCache. Returns
// the count of corrupt run files diskListNewestFirst skipped during the
// scan so the caller can emit a single aggregate slog AFTER the locks
// drop. Callers MUST NOT hold any runStore lock; this function takes
// jobLock and entry.mu internally.
func (s *runStore) warmCacheLocked(jobID string) int {
	lock := s.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()

	v, _ := s.recentCache.LoadOrStore(jobID, &recentCacheEntry{})
	entry := v.(*recentCacheEntry)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.warm {
		return 0 // another goroutine warmed it during our wait
	}
	rows, corruptCount := s.diskListNewestFirst(jobID, s.keepCount, time.Time{})
	entry.ringSeed(rows, s.keepCount)
	entry.warm = true
	return corruptCount
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
	if n > DefaultRunsKeepCount {
		n = DefaultRunsKeepCount
	}
	// Cache-warm fast path: read SessionIDs directly off the ring under
	// entry.mu without materialising a CronRunSummary slice. Mirrors the
	// before=zero branch of List → cacheGet but skips ringSnapshot's
	// per-row value copy.
	if v, ok := s.recentCache.Load(jobID); ok {
		entry := v.(*recentCacheEntry)
		entry.mu.Lock()
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
			entry.mu.Unlock()
			return out
		}
		entry.mu.Unlock()
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
	return readAllIntoReader(f, buf)
}

// readAllIntoReader is the testable core of readAllInto. It accepts an
// io.Reader so unit tests can inject a fake reader that repeatedly returns
// (0, nil) to exercise the zero-progress guard (R171023-CR-007).
//
// The guard breaks out of the loop after zeroProgressLimit consecutive
// (0, nil) reads so the function does not hang on io.Reader implementations
// that are contractually allowed to return (0, nil) (e.g., certain FUSE
// file systems). os.File on Linux follows POSIX and will not do this in
// practice, but defence-in-depth applies here.
const zeroProgressLimit = 2

func readAllIntoReader(r io.Reader, buf []byte) ([]byte, error) {
	zeroCount := 0
	for {
		if len(buf) == cap(buf) {
			buf = append(buf, 0)[:len(buf)]
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buf, nil
			}
			return buf, err
		}
		if n == 0 {
			zeroCount++
			if zeroCount >= zeroProgressLimit {
				return buf, io.ErrNoProgress
			}
		} else {
			zeroCount = 0
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
	toRemove := make([]string, 0, len(items))
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
		// R250-PERF-9 (#1112): pre-warm the recentCache for this job in the
		// same cold-start goroutine, right after trim settled its on-disk
		// state. Without this, the FIRST dashboard RecentRuns poll after a
		// process restart cold-warms every entry serially on the request
		// path (ReadDir + per-file Lstat + ReadFile + json.Unmarshal up to
		// keepCount per job) — multi-second first-poll latency operators see
		// when the dashboard reconnects. warmCacheLocked is idempotent (skips
		// when entry.warm) so a concurrent Append-driven warm that already
		// fired is a cheap no-op here. The extra per-job ReadDir at startup
		// is off the hot path and bounded by maxJobsHardCap. Cancelling the
		// GC ctx between jobs (checked at loop top) also short-circuits
		// remaining warms, so Stop stays prompt.
		if warmCorrupt := s.warmCacheLocked(jobID); warmCorrupt > 0 {
			slog.Warn("cron runstore: cold-start warm skipped corrupt files",
				"count", warmCorrupt, "dir", filepath.Join(s.root, jobID))
		}
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
