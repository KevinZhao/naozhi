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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/textutil"
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
// Errors are surfaced via slog rather than propagated to callers (cron
// never blocks on history failure — RFC §4.2). The exception: GC's
// cumulative result is logged but not aborted on a single file failure.
type runStore struct {
	root         string
	keepCount    int
	keepWindow   time.Duration
	maxRunBytes  int64
	jobLocks     sync.Map // jobID -> *sync.Mutex
	disabled     bool     // true when StorePath is empty (tests / no-persist)
	enableTrimGC bool     // true in production; tests can disable for determinism

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

// recentCacheEntry is the cached newest-first snapshot for one job. capLen
// is the populated portion; the underlying slice may be over-allocated to
// `keepCount` so head-pushes don't reallocate.
type recentCacheEntry struct {
	mu   sync.Mutex
	runs []CronRunSummary // newest-first
	warm bool             // false until first warm() pass; List/Recent will lazy-warm
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

// IsValidID reports whether s matches the lowercase-hex shape produced
// by generateRunID / generateID (currently 16 hex chars; upper bound 64
// reserved for a future schema bump). Imposed at parse time so a stray
// non-run file in runs/<jobID>/ cannot poison List output, and exposed
// so HTTP handlers can fail-fast at the boundary instead of forwarding
// non-hex IDs that the store would silently swallow as an empty result.
// R221-FIX-P1-2.
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

// runIDPattern is the package-internal alias kept for readability at
// existing call sites.
var runIDPattern = IsValidID

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

// Append writes one run record to disk and trims the per-job ring.
// Errors are logged, never returned: cron must not block history failure.
func (s *runStore) Append(run *CronRun) {
	if s == nil || s.disabled || run == nil || run.JobID == "" || run.RunID == "" {
		return
	}
	if !runIDPattern(run.RunID) {
		slog.Warn("cron run: skipping append with invalid run_id", "run_id", run.RunID)
		return
	}
	if !runIDPattern(run.JobID) {
		// jobID 历史上是 16-hex；非 hex 可能是测试 fixture / 篡改文件。
		// 拒绝 append 而非创建可疑目录。
		slog.Warn("cron run: skipping append with non-hex job_id", "job_id", run.JobID)
		return
	}
	lock := s.jobLock(run.JobID)
	lock.Lock()
	defer lock.Unlock()

	dir := filepath.Join(s.root, run.JobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
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
		shrunk.Result = truncateForRetry(shrunk.Result, 256)
		shrunk.Prompt = truncateForRetry(shrunk.Prompt, 256)
		shrunk.ErrorMsg = truncateForRetry(shrunk.ErrorMsg, 256)
		if data2, err2 := json.Marshal(&shrunk); err2 == nil && int64(len(data2)) <= s.maxRunBytes {
			data = data2
		} else {
			// 再失败就放弃；已经写日志，下次 GC 会保持现状。
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
	if s.enableTrimGC {
		s.trimJobLocked(run.JobID, time.Now())
	}
}

// cacheHeadPush prepends summary to the recentCache slice for jobID. The
// caller must hold jobLock(jobID) so the push is serialised against
// concurrent Recent / List reads. No-op when the cache entry is not yet
// warm (List/Recent will populate from disk on first miss).
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
	// Newest-first: prepend. Cap to keepCount to bound memory; entries
	// beyond cap are trimmed by trimJobLocked at the same time.
	entry.runs = append([]CronRunSummary{summary}, entry.runs...)
	if len(entry.runs) > s.keepCount {
		entry.runs = entry.runs[:s.keepCount]
	}
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
		out := s.copySummariesLocked(entry.runs, limit)
		entry.mu.Unlock()
		return out, true
	}
	entry.mu.Unlock()

	// Cold cache: warm from disk under jobLock so concurrent Append's
	// cacheHeadPush observes the freshly-warmed slice (and would no-op
	// before, but warm is now true).
	if err := s.warmCache(jobID); err != nil {
		// Disk error — caller falls back to direct disk read.
		return nil, false
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !entry.warm {
		return nil, false
	}
	return s.copySummariesLocked(entry.runs, limit), true
}

// warmCache populates the recentCache for jobID by reading the on-disk
// runs/<jobID>/ directory and parsing each .json file. Holds the per-job
// disk lock so a concurrent Append can't race the warm pass.
func (s *runStore) warmCache(jobID string) error {
	lock := s.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()

	v, _ := s.recentCache.LoadOrStore(jobID, &recentCacheEntry{})
	entry := v.(*recentCacheEntry)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.warm {
		return nil // another goroutine warmed it during our wait
	}
	rows := s.diskListNewestFirst(jobID, s.keepCount, time.Time{})
	entry.runs = rows
	entry.warm = true
	return nil
}

// copySummariesLocked returns a defensive copy of up to limit entries
// from the cache slice. Caller holds entry.mu.
func (s *runStore) copySummariesLocked(src []CronRunSummary, limit int) []CronRunSummary {
	if limit <= 0 || limit > len(src) {
		limit = len(src)
	}
	out := make([]CronRunSummary, limit)
	copy(out, src[:limit])
	return out
}

// cacheInvalidate forgets the cache entry for jobID. Used by DeleteJob.
func (s *runStore) cacheInvalidate(jobID string) {
	s.recentCache.Delete(jobID)
}

// truncateForRetry shrinks a string for the over-cap retry path. Keeps
// the prefix + "…[truncated]" sentinel so the UI can still indicate
// data was lost without forcing a code change. maxRunes counts runes,
// not bytes — a byte-level slice would split multi-byte UTF-8 (Chinese
// prompts/results are common here) and produce a malformed string that
// json.Marshal silently re-encodes as U+FFFD; in the worst case the
// second marshal still exceeds maxRunBytes and the run record is never
// persisted. Rune-aware truncation closes that hole. R221-FIX-P0-1.
func truncateForRetry(s string, maxRunes int) string {
	shrunk := textutil.TruncateRunesNoEllipsis(s, maxRunes)
	if shrunk == s {
		return s
	}
	return shrunk + "…[truncated]"
}

// List returns up to limit summaries for jobID, newest first. before is
// a unix-ms cutoff: only runs with StartedAt < before are returned (paging).
// Zero before = no cutoff. Errors during read are logged and the entry
// skipped; callers always receive a (possibly partial) list.
func (s *runStore) List(jobID string, limit int, before time.Time) []CronRunSummary {
	if s == nil || s.disabled || jobID == "" {
		return nil
	}
	if !runIDPattern(jobID) {
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
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		runID := strings.TrimSuffix(name, ".json")
		if !runIDPattern(runID) {
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
	sort.Slice(items, func(i, j int) bool { return items[i].mtime.After(items[j].mtime) })

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
	if !runIDPattern(jobID) || !runIDPattern(runID) {
		return nil, fs.ErrNotExist
	}
	path := filepath.Join(s.root, jobID, runID+".json")
	return s.readRun(path)
}

// readRun parses a single run file. Returns ErrCorruptRun on parse
// failure or oversize; fs.ErrNotExist propagates unchanged.
func (s *runStore) readRun(path string) (*CronRun, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > s.maxRunBytes {
		return nil, fmt.Errorf("%w: %d bytes > %d cap", ErrCorruptRun, len(data), s.maxRunBytes)
	}
	var run CronRun
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptRun, err)
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
	if !runIDPattern(jobID) {
		return
	}
	lock := s.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()
	dir := filepath.Join(s.root, jobID)
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("cron run: delete job runs subtree failed", "dir", dir, "err", err)
	}
	s.cacheInvalidate(jobID)
}

// trimJobLocked enforces the per-job retention policy. Caller must hold
// jobLock(jobID). now is passed in so tests can inject deterministic
// "current time" without touching time.Now().
//
// Policy: keep ALL runs satisfying BOTH (rank ≤ keepCount) AND
// (age ≤ keepWindow). Either condition violated → delete. AND-vs-OR
// is the user-confirmed choice in the RFC chat (§4.3): high-frequency
// jobs get capped by count; low-frequency jobs by window.
func (s *runStore) trimJobLocked(jobID string, now time.Time) {
	dir := filepath.Join(s.root, jobID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type item struct {
		path  string
		mtime time.Time
	}
	items := make([]item, 0, len(entries))
	cutoff := now.Add(-s.keepWindow)
	anyExpired := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		runID := strings.TrimSuffix(name, ".json")
		if !runIDPattern(runID) {
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
		items = append(items, item{path: filepath.Join(dir, name), mtime: mtime})
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
	// Sort newest first so rank checking is index-based.
	slices.SortFunc(items, func(a, b item) int { return cmp.Compare(b.mtime.UnixNano(), a.mtime.UnixNano()) })
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
	// cache slice is newest-first, so iterate and stop at first old.
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
	// Allocate a fresh backing array — `entry.runs[:0]` would alias and
	// any caller that retains the slice (Recent's defensive copy is in
	// place today, but a future code path might not) would observe the
	// trim. R221-FIX-P0-2.
	keep := make([]CronRunSummary, 0, len(entry.runs))
	for i, r := range entry.runs {
		if i >= s.keepCount {
			break
		}
		ts := r.EndedAt
		if ts.IsZero() {
			ts = r.StartedAt
		}
		if ts.Before(cutoff) {
			break
		}
		keep = append(keep, r)
	}
	entry.runs = keep
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
			slog.Debug("cron run: trimAll readdir", "root", s.root, "err", err)
		}
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		jobID := e.Name()
		if !runIDPattern(jobID) {
			continue
		}
		lock := s.jobLock(jobID)
		lock.Lock()
		s.trimJobLocked(jobID, now)
		lock.Unlock()
	}
}
