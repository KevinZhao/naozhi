package runhistory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/osutil/jsonfile"
	"github.com/naozhi/naozhi/internal/runlog"
)

// Defaults mirror cron's retention intent with a smaller ring: session keys
// are far more numerous than cron jobs, so 50 keeps resident memory bounded.
const (
	DefaultKeepCount  = 50
	DefaultKeepWindow = 30 * 24 * time.Hour
)

// Store persists SessionRun records to disk and memoises the newest-N per
// session in an in-memory ring. Layout:
//
//	<runsRoot>/<sha256(sessionKey)[:16]>/<run_id>.json
//
// No index.json: List/Recent serve from the ring, warmed lazily from disk.
// A nil or disabled Store is a no-op, so callers never nil-check.
//
// The tree itself — root validation, the per-session directory, the atomic
// record write and the per-session lock — belongs to internal/runlog, shared
// with cron's run store (#2709). What stays here is what actually differs: the
// record type, the ring, and the warm-time retention pass.
type Store struct {
	layout     *runlog.Layout
	keepCount  int
	keepWindow time.Duration
	disabled   bool

	mu      sync.Mutex
	entries map[string]*sessionEntry // dirHash -> entry

	// Async write path: a single worker drains a bounded channel so the
	// conversation goroutine never pays the fsync; a full channel drops.
	asyncCh   chan SessionRun
	closeOnce sync.Once
	closed    atomic.Bool
	wg        sync.WaitGroup
	dropTotal atomic.Int64
}

// asyncQueueDepth absorbs bursts of many sessions finishing in the same tick.
const asyncQueueDepth = 256

// sessionEntry owns one session's recent ring. The mutex that guards it (and
// the session's disk subtree, which trim mutates) is runlog's per-owner lock,
// so both run stores key that lock through one API.
type sessionEntry struct {
	ring   []SessionRun // newest-first, len <= keepCount
	warmed bool
}

// NewStore returns a Store rooted at runsRoot, which the caller names via
// datadir.Layout.SessionRunsRoot() — this package no longer appends the
// "session-runs" segment itself, so the store and the cost reporter that reads
// the same directory cannot disagree about it (#2641). Empty runsRoot disables
// persistence; keepCount/keepWindow <= 0 use the package defaults.
func NewStore(runsRoot string, keepCount int, keepWindow time.Duration) *Store {
	if runsRoot == "" {
		return &Store{disabled: true}
	}
	if keepCount <= 0 {
		keepCount = DefaultKeepCount
	}
	if keepWindow <= 0 {
		keepWindow = DefaultKeepWindow
	}
	layout := runlog.New(runlog.Options{
		Root:         runsRoot,
		Label:        "session run",
		OwnerDirName: dirHashFor,
	})
	if !layout.Enabled() {
		// A symlinked or non-directory root disables persistence rather than
		// writing through it; runlog logged the reason.
		return &Store{disabled: true}
	}
	s := &Store{
		layout:     layout,
		keepCount:  keepCount,
		keepWindow: keepWindow,
		entries:    make(map[string]*sessionEntry),
		asyncCh:    make(chan SessionRun, asyncQueueDepth),
	}
	s.wg.Add(1)
	go s.worker()
	return s
}

// worker performs the blocking disk writes off the caller's goroutine.
func (s *Store) worker() {
	defer s.wg.Done()
	for run := range s.asyncCh {
		s.Append(run)
	}
}

// AppendAsync enqueues a run for background persistence. Non-blocking: a
// full queue drops the record and bumps DropTotal. Safe on a nil/disabled Store.
func (s *Store) AppendAsync(run SessionRun) {
	if s == nil || s.disabled || s.asyncCh == nil || s.closed.Load() {
		return
	}
	// closed.Load is a fast-path; the recover covers Close racing between the
	// check and the send (a closed-channel send panics).
	defer func() { _ = recover() }()
	select {
	case s.asyncCh <- run:
	default:
		n := s.dropTotal.Add(1)
		slog.Warn("session run: async queue full, dropping record", "session_key_hash", dirHashFor(run.SessionKey), "drop_total", n)
	}
}

// Close stops the worker after flushing queued records. Idempotent.
func (s *Store) Close() {
	if s == nil || s.disabled {
		return
	}
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		close(s.asyncCh)
	})
	s.wg.Wait()
}

// DropTotal returns the number of records dropped due to a full async queue.
func (s *Store) DropTotal() int64 {
	if s == nil {
		return 0
	}
	return s.dropTotal.Load()
}

// dirHashFor maps a sessionKey (':' and user-controlled content) to a
// filesystem-safe directory name, defending against path traversal.
func dirHashFor(sessionKey string) string {
	sum := sha256.Sum256([]byte(sessionKey))
	return hex.EncodeToString(sum[:8]) // 16 hex chars
}

func (s *Store) entryFor(dirHash string) *sessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[dirHash]
	if e == nil {
		e = &sessionEntry{}
		s.entries[dirHash] = e
	}
	return e
}

// Append writes one run record to disk and pushes it onto the session's
// ring. Errors are logged, never returned: history must never fail the
// user's conversation. Safe on a nil/disabled Store.
func (s *Store) Append(run SessionRun) {
	if s == nil || s.disabled {
		return
	}
	if !isValidRunID(run.RunID) || run.SessionKey == "" {
		slog.Warn("session run: skipping append with invalid id/key", "run_id", run.RunID)
		return
	}
	if run.DurationMS < 0 {
		run.DurationMS = 0 // monotonic-clock skew guard (cron parity)
	}

	dirHash := dirHashFor(run.SessionKey)
	e := s.entryFor(dirHash)

	lock := s.layout.Lock(run.SessionKey)
	lock.Lock()
	defer lock.Unlock()

	if !e.warmed {
		s.warmLocked(e, dirHash)
	}

	data, err := json.Marshal(run)
	if err != nil {
		slog.Warn("session run: marshal failed", "run_id", run.RunID, "err", err)
		return
	}
	// runlog re-validates the per-session directory on every write (a symlink
	// swapped in after the first append is still caught) and splits the
	// write-failure counters so ENOSPC is distinguishable from EACCES.
	if err := s.layout.WriteRecord(run.SessionKey, run.RunID, data); err != nil {
		slog.Warn("session run: write failed", "run_id", run.RunID, "err", err)
		return
	}

	e.ring = append([]SessionRun{run}, e.ring...)
	s.trimLocked(e, s.dirPath(dirHash))
}

// dirPath names a session's subtree. The layout owns the mapping; this is the
// spelling the trim/warm passes need for their ReadDir + Remove calls.
func (s *Store) dirPath(dirHash string) string {
	return filepath.Join(s.layout.Root(), dirHash)
}

// WriteFailedTotals returns the split record-write failure counters. Append
// cannot fail the caller's turn, so these plus the Warn log are the only
// operator-visible signal that history is being lost.
func (s *Store) WriteFailedTotals() (diskFull, other int64) {
	if s == nil || s.disabled {
		return 0, 0
	}
	return s.layout.WriteFailedTotals()
}

// trimLocked enforces keepCount on the ring and disk. Caller holds the session lock.
func (s *Store) trimLocked(e *sessionEntry, dir string) {
	if len(e.ring) <= s.keepCount {
		return
	}
	for _, evicted := range e.ring[s.keepCount:] {
		_ = os.Remove(filepath.Join(dir, evicted.RunID+".json"))
	}
	e.ring = e.ring[:s.keepCount]
}

// warmLocked scans the session's on-disk directory into the ring, applying
// keepWindow and keepCount. Caller holds the session lock. Expired files are deleted as
// they are skipped: warm is the only path touching a subtree after the
// session stops appending, so otherwise they accumulate unbounded (#2225).
func (s *Store) warmLocked(e *sessionEntry, dirHash string) {
	e.warmed = true
	dir := s.dirPath(dirHash)
	ents, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("session run: readdir failed", "dir", dir, "err", err)
		}
		return
	}
	cutoff := time.Now().Add(-s.keepWindow)
	runs := make([]SessionRun, 0, len(ents))
	for _, ent := range ents {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if filepath.Ext(name) != ".json" {
			continue
		}
		id := name[:len(name)-len(".json")]
		if !isValidRunID(id) {
			continue
		}
		var run SessionRun
		if err := readRunFile(filepath.Join(dir, name), &run); err != nil {
			continue
		}
		if run.StartedAt.Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt.After(runs[j].StartedAt) })
	if len(runs) > s.keepCount {
		runs = runs[:s.keepCount]
	}
	e.ring = runs
}

// maxRunFileBytes caps one run record. A SessionRun is a handful of scalars
// plus a truncated result string, so 1 MiB is far above any legitimate payload
// and bounds what a tampered or half-written file can allocate.
const maxRunFileBytes = 1 << 20

// readRunFile reads one run record. A file that fails to parse is moved aside as
// .corrupt.<ts> rather than being skipped in place: warmLocked answered the old
// error with `continue`, so an unparseable record was re-read on every warm and
// never removed — the retention GC below the skip could not reach it either.
func readRunFile(path string, dst *SessionRun) error {
	run, out, err := jsonfile.Load[SessionRun](path, jsonfile.Options{
		MaxBytes: maxRunFileBytes,
		Label:    "session run record",
	})
	if err != nil {
		return err
	}
	if out != jsonfile.Parsed {
		return errRunFileUnusable
	}
	*dst = run
	return nil
}

// errRunFileUnusable reports a record that is absent, empty, or was moved aside
// as corrupt — in every case there is nothing to load and nothing left to clean.
var errRunFileUnusable = errors.New("runhistory: run record unusable")

// Recent returns up to n newest-first runs (n <= 0: all cached) as a fresh copy.
func (s *Store) Recent(sessionKey string, n int) []SessionRun {
	if s == nil || s.disabled || sessionKey == "" {
		return nil
	}
	dirHash := dirHashFor(sessionKey)
	e := s.entryFor(dirHash)
	lock := s.layout.Lock(sessionKey)
	lock.Lock()
	defer lock.Unlock()
	if !e.warmed {
		s.warmLocked(e, dirHash)
	}
	if n <= 0 || n > len(e.ring) {
		n = len(e.ring)
	}
	out := make([]SessionRun, n)
	copy(out, e.ring[:n])
	return out
}

// List returns newest-first runs started strictly before `before` (zero:
// no bound), capped at limit (<=0 or > keepCount: keepCount). Fresh copy.
func (s *Store) List(sessionKey string, limit int, before time.Time) []SessionRun {
	all := s.Recent(sessionKey, 0)
	if len(all) == 0 {
		return all
	}
	if limit <= 0 || limit > s.keepCount {
		limit = s.keepCount
	}
	out := make([]SessionRun, 0, limit)
	for _, r := range all {
		if !before.IsZero() && !r.StartedAt.Before(before) {
			continue
		}
		out = append(out, r)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// Stats returns the aggregate over the newest keepCount runs of the session.
func (s *Store) Stats(sessionKey string) SessionRunStats {
	return ComputeStats(s.Recent(sessionKey, 0))
}

// Invalidate drops a session's cached ring when the session is reset /
// evicted / removed; on-disk records stay. Safe on nil/disabled.
func (s *Store) Invalidate(sessionKey string) {
	if s == nil || s.disabled || sessionKey == "" {
		return
	}
	dirHash := dirHashFor(sessionKey)
	s.mu.Lock()
	delete(s.entries, dirHash)
	s.mu.Unlock()
	// The ring is gone, so the lock that guarded it has nothing left to
	// serialise: drop it too rather than leaking one mutex per session for the
	// process lifetime.
	s.layout.ForgetOwner(sessionKey)
}
