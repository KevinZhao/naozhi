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

// Defaults mirror cron's retention intent (limits.go) but with a smaller
// ring: session keys are far more numerous than cron jobs (hundreds–
// thousands vs a handful), so a 200-slot resident ring per session would
// scale memory poorly. 50 is ample for the timeline + stats.
const (
	DefaultKeepCount  = 50
	DefaultKeepWindow = 30 * 24 * time.Hour

	runFilePerm = 0o600
	dirPerm     = 0o700
)

// Store persists SessionRun records to disk and memoises the newest-N per
// session in an in-memory ring. Layout (rooted at <root>/session-runs):
//
//	session-runs/
//	    <sha256(sessionKey)[:16]>/
//	        <run_id>.json     # one record per run; ~150 B typical
//
// There is no index.json: List/Recent serve from the ring, which warms
// lazily from disk on first access. A nil or disabled Store is a no-op, so
// callers never need to nil-check.
type Store struct {
	root       string // <...>/session-runs ; "" disables persistence
	keepCount  int
	keepWindow time.Duration
	disabled   bool

	mu      sync.Mutex
	entries map[string]*sessionEntry // dirHash -> entry

	// Async write path. AppendAsync hands records to a single background
	// worker via a bounded channel so the user's conversation goroutine
	// never pays the fsync of WriteFileAtomic. A full channel drops the
	// record (best-effort history) rather than blocking the hot path.
	asyncCh   chan SessionRun
	closeOnce sync.Once
	closed    atomic.Bool
	wg        sync.WaitGroup
	dropTotal atomic.Int64
}

// asyncQueueDepth bounds the pending-write channel. Sized to absorb short
// bursts (many sessions finishing a turn in the same tick) without blocking;
// overflow drops to keep the hot path non-blocking.
const asyncQueueDepth = 256

// sessionEntry owns one session's recent ring plus the lock serialising its
// disk subtree. ring holds newest-first summaries; warmed reports whether
// the on-disk directory has been scanned into ring yet.
type sessionEntry struct {
	mu     sync.Mutex
	ring   []SessionRun // newest-first, len <= keepCount
	warmed bool
}

// NewStore returns a Store rooted at <storeDir>/session-runs. An empty
// storeDir disables persistence (used in tests / no-persist configs).
// keepCount/keepWindow <= 0 fall back to the package defaults.
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

// worker drains the async channel, performing the (blocking) disk write off
// the caller's goroutine. Exits when the channel is closed by Close.
func (s *Store) worker() {
	defer s.wg.Done()
	for run := range s.asyncCh {
		s.Append(run)
	}
}

// AppendAsync enqueues a run for background persistence. Non-blocking: if the
// queue is full the record is dropped (best-effort) and a counter bumped, so
// the user's conversation path is never stalled by history I/O. Safe on a
// nil/disabled Store.
func (s *Store) AppendAsync(run SessionRun) {
	if s == nil || s.disabled || s.asyncCh == nil || s.closed.Load() {
		return
	}
	// closed.Load above is a best-effort fast-path; the recover guards the
	// genuine race where Close runs between the check and the send. A closed
	// channel send panics — recovering keeps a shutdown-time record loss from
	// taking down the conversation goroutine.
	defer func() { _ = recover() }()
	select {
	case s.asyncCh <- run:
	default:
		n := s.dropTotal.Add(1)
		slog.Warn("session run: async queue full, dropping record", "session_key_hash", dirHashFor(run.SessionKey), "drop_total", n)
	}
}

// Close stops the background worker and flushes records already queued.
// Idempotent. Records enqueued after Close are ignored.
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

// DropTotal returns the number of records dropped due to a full async queue
// (observability / tests).
func (s *Store) DropTotal() int64 {
	if s == nil {
		return 0
	}
	return s.dropTotal.Load()
}

// dirHashFor maps a sessionKey (which contains ':' and user-controlled
// content) to a filesystem-safe directory name, defending against path
// traversal.
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
// recent ring, trimming by count. Errors are logged, never returned: a
// history write must never block or fail the user's conversation. Safe to
// call on a nil/disabled Store.
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

	// Push newest-first and trim to keepCount.
	e.ring = append([]SessionRun{run}, e.ring...)
	s.trimLocked(e, dir)
}

// trimLocked enforces keepCount on the ring and deletes the corresponding
// on-disk files for evicted entries. Caller holds e.mu.
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
// the keepWindow age filter and keepCount cap. Caller holds e.mu.
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
			continue
		}
		runs = append(runs, run)
	}
	// newest-first
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

// Recent returns up to n newest-first runs for the session. n <= 0 returns
// all cached (up to keepCount). The returned slice is a fresh copy — callers
// may sort/filter freely.
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

// List returns newest-first runs for the session, optionally paginating:
// only runs started strictly before `before` are returned (zero `before`
// means no upper bound), capped at limit (<=0 or > keepCount means
// keepCount). A fresh copy is returned.
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

// Invalidate drops a session's cached ring, freeing the resident slots when
// the session is reset / evicted / removed. The on-disk records are left
// intact (subject to keepWindow GC on next warm). Safe on nil/disabled.
func (s *Store) Invalidate(sessionKey string) {
	if s == nil || s.disabled || sessionKey == "" {
		return
	}
	dirHash := dirHashFor(sessionKey)
	s.mu.Lock()
	delete(s.entries, dirHash)
	s.mu.Unlock()
}
