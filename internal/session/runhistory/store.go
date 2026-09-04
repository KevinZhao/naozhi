package runhistory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// Defaults mirror cron's retention intent with a smaller ring: session keys
// are far more numerous than cron jobs, so 50 keeps resident memory bounded.
const (
	DefaultKeepCount  = 50
	DefaultKeepWindow = 30 * 24 * time.Hour

	runFilePerm = 0o600
	dirPerm     = 0o700
)

// Store persists SessionRun records to disk and memoises the newest-N per
// session in an in-memory ring. Layout:
//
//	session-runs/<sha256(sessionKey)[:16]>/<run_id>.json
//
// No index.json: List/Recent serve from the ring, warmed lazily from disk.
// A nil or disabled Store is a no-op, so callers never nil-check.
type Store struct {
	root       string // <...>/session-runs ; "" disables persistence
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

// sessionEntry owns one session's recent ring plus the lock for its disk subtree.
type sessionEntry struct {
	mu     sync.Mutex
	ring   []SessionRun // newest-first, len <= keepCount
	warmed bool
}

// NewStore returns a Store rooted at <storeDir>/session-runs. Empty storeDir
// disables persistence; keepCount/keepWindow <= 0 use the package defaults.
func NewStore(storeDir string, keepCount int, keepWindow time.Duration) *Store {
	if storeDir == "" {
		return &Store{disabled: true}
	}
	if keepCount <= 0 {
		keepCount = DefaultKeepCount
	}
	if keepWindow <= 0 {
		keepWindow = DefaultKeepWindow
	}
	s := &Store{
		root:       filepath.Join(storeDir, "session-runs"),
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

	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.warmed {
		s.warmLocked(e, dirHash)
	}

	dir := filepath.Join(s.root, dirHash)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		slog.Warn("session run: mkdir failed", "dir", dir, "err", err)
		return
	}
	data, err := json.Marshal(run)
	if err != nil {
		slog.Warn("session run: marshal failed", "run_id", run.RunID, "err", err)
		return
	}
	path := filepath.Join(dir, run.RunID+".json")
	if err := osutil.WriteFileAtomic(path, data, runFilePerm); err != nil {
		slog.Warn("session run: write failed", "path", path, "err", err)
		return
	}

	e.ring = append([]SessionRun{run}, e.ring...)
	s.trimLocked(e, dir)
}

// trimLocked enforces keepCount on the ring and disk. Caller holds e.mu.
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
// keepWindow and keepCount. Caller holds e.mu. Expired files are deleted as
// they are skipped: warm is the only path touching a subtree after the
// session stops appending, so otherwise they accumulate unbounded (#2225).
func (s *Store) warmLocked(e *sessionEntry, dirHash string) {
	e.warmed = true
	dir := filepath.Join(s.root, dirHash)
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

func readRunFile(path string, dst *SessionRun) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}

// Recent returns up to n newest-first runs (n <= 0: all cached) as a fresh copy.
func (s *Store) Recent(sessionKey string, n int) []SessionRun {
	if s == nil || s.disabled || sessionKey == "" {
		return nil
	}
	dirHash := dirHashFor(sessionKey)
	e := s.entryFor(dirHash)
	e.mu.Lock()
	defer e.mu.Unlock()
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
}
