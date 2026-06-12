package discovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// RetiredStore tracks the wall-clock instant a session left the live sidebar
// (Router.Reset / Router.Remove → notifyKeyRetired). The dashboard history
// drawer sorts by this instant so "the most recently closed session" appears
// at the top regardless of when its JSONL was last appended — JSONL mtime is
// only the last-message timestamp, which can be days older than the moment
// the operator actually closed the panel.
//
// Persisted as a single JSON file alongside sessions.json so the order
// survives naozhi restarts. Best-effort: a corrupt/missing file degrades to
// "no entries known", and the dashboard falls back to LastActive ordering.
//
// Concurrency: all public methods acquire mu. Save() is debounced — callers
// invoke MarkRetired in the lifecycle hot path (no I/O on the critical
// section) and a periodic flusher (or explicit Flush at shutdown) writes to
// disk. The on-disk file may lag the in-memory state by up to flushInterval;
// this is acceptable because RetiredAt is a UX hint, not a correctness
// invariant.
type RetiredStore struct {
	path string

	mu      sync.Mutex
	entries map[string]int64 // sessionID → unix ms
	dirty   bool

	// gen counts mutations to entries. Save snapshots gen under the lock
	// alongside the entries copy, then after the unlocked marshal+write only
	// clears dirty when gen is unchanged. This closes the window where a
	// concurrent MarkRetired (lifecycle hot path, runs while Save is doing its
	// unlocked WriteFileAtomic+fsync) sets dirty=true for an entry that the
	// in-flight write does NOT include — without the gen guard Save would
	// unconditionally clear dirty and that entry would never be flushed,
	// losing it across a restart. R20260610-085718-LB-6 (#2011).
	gen uint64

	// maxEntries caps how many sessionIDs the store will retain after a
	// Prune call. Without a cap a long-running deployment grows the JSON
	// file without bound — the history drawer only displays sessions
	// from the last 7 days but the store has no inherent expiry knob
	// because RetiredAt timestamps may post-date the corresponding JSONL
	// mtime by weeks (tab leaves the panel long after the chat ended).
	// Default 4096 = ~80 KB on disk; a busy operator closes ~50 sessions
	// per week so the cap protects against pathological cases without
	// trimming legitimate history.
	maxEntries int
}

// retiredStoreFileV1 is the on-disk schema. Version field reserved for
// future migrations — readers tolerate unknown fields via json.Unmarshal's
// default behaviour, writers always emit the current version.
type retiredStoreFileV1 struct {
	Version int              `json:"version"`
	Entries map[string]int64 `json:"entries"`
}

const retiredStoreVersion = 1

// DefaultRetiredStoreMaxEntries is the cap used when callers don't override
// it via NewRetiredStoreWithCap. Exposed as a constant so tests can assert
// the production value without re-deriving the math.
const DefaultRetiredStoreMaxEntries = 4096

// NewRetiredStore constructs a store backed by `path`. An empty path
// disables persistence (in-memory only) — used by tests and by deployments
// without a state directory configured. Returns a usable store even when
// the file does not yet exist; the first Save() will create it.
//
// Load errors are logged via the error return but do not block construction:
// a corrupt store should not prevent naozhi from starting, since RetiredAt
// is purely a UX sort hint.
func NewRetiredStore(path string) (*RetiredStore, error) {
	return NewRetiredStoreWithCap(path, DefaultRetiredStoreMaxEntries)
}

// NewRetiredStoreWithCap is NewRetiredStore with an explicit entry cap.
// Callers passing cap <= 0 get the default cap.
func NewRetiredStoreWithCap(path string, cap int) (*RetiredStore, error) {
	if cap <= 0 {
		cap = DefaultRetiredStoreMaxEntries
	}
	rs := &RetiredStore{
		path:       path,
		entries:    make(map[string]int64),
		maxEntries: cap,
	}
	if path == "" {
		return rs, nil
	}
	if err := rs.load(); err != nil {
		// Caller decides whether to surface; we still return a valid empty store.
		return rs, err
	}
	return rs, nil
}

func (rs *RetiredStore) load() error {
	data, err := os.ReadFile(rs.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read retired store: %w", err)
	}
	if len(data) == 0 {
		return nil
	}
	var file retiredStoreFileV1
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("parse retired store: %w", err)
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if file.Entries != nil {
		rs.entries = file.Entries
	}
	return nil
}

// MarkRetired records `now` as the instant `sessionID` left the live sidebar.
// Idempotent across multiple calls — the most recent timestamp wins so a
// Reset → Remove (rare but legal) sequence reports the Remove instant.
// sessionID may be empty for sessions that never had a UUID assigned (e.g.
// resume failed before the CLI returned init); callers should skip those.
//
// Marks dirty for the next Save(); does not perform I/O.
func (rs *RetiredStore) MarkRetired(sessionID string, now time.Time) {
	if sessionID == "" {
		return
	}
	ms := now.UnixMilli()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if existing, ok := rs.entries[sessionID]; ok && existing >= ms {
		// Clock skew or duplicate retirement signal — keep the larger
		// timestamp so retirement order remains monotonic.
		return
	}
	rs.entries[sessionID] = ms
	rs.dirty = true
	rs.gen++
}

// Get returns the recorded retirement time for sessionID in unix ms, or 0
// when no entry exists. Zero is the dashboard's "fall back to LastActive"
// signal; callers must not treat it as a valid timestamp.
func (rs *RetiredStore) Get(sessionID string) int64 {
	if sessionID == "" {
		return 0
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.entries[sessionID]
}

// Snapshot returns a copy of all sessionID→retiredAt pairs. The caller owns
// the returned map and may iterate without holding rs.mu.
func (rs *RetiredStore) Snapshot() map[string]int64 {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make(map[string]int64, len(rs.entries))
	for k, v := range rs.entries {
		out[k] = v
	}
	return out
}

// Save writes the current map atomically to disk via tmpfile + rename.
// No-op when the path is empty or the store has not been mutated since the
// last successful Save. Returns nil on a no-op.
//
// Callers may invoke Save from a periodic ticker (the typical wiring) and
// once at shutdown to flush the final retirement.
func (rs *RetiredStore) Save() error {
	rs.mu.Lock()
	if rs.path == "" || !rs.dirty {
		rs.mu.Unlock()
		return nil
	}
	// Snapshot under lock so the JSON encode runs on a stable copy and
	// concurrent MarkRetired calls don't race the marshaller. Capture gen
	// alongside the snapshot so we can detect mutations that land during the
	// unlocked write window (see the gen field doc / #2011).
	snap := make(map[string]int64, len(rs.entries))
	for k, v := range rs.entries {
		snap[k] = v
	}
	savedGen := rs.gen
	rs.mu.Unlock()

	file := retiredStoreFileV1{
		Version: retiredStoreVersion,
		Entries: snap,
	}
	data, err := json.Marshal(file)
	if err != nil {
		return fmt.Errorf("marshal retired store: %w", err)
	}

	dir := filepath.Dir(rs.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir retired store: %w", err)
	}
	// osutil.WriteFileAtomic handles tmpfile + fsync + rename + dir-fsync,
	// replacing the inlined boilerplate (and adding a directory fsync that
	// the previous code lacked).
	if err := osutil.WriteFileAtomic(rs.path, data, 0o600); err != nil {
		return fmt.Errorf("write retired store: %w", err)
	}

	rs.mu.Lock()
	// Only clear dirty when no mutation landed during the unlocked write
	// window. A concurrent MarkRetired bumps gen for an entry the just-written
	// snapshot did not contain; clearing dirty unconditionally would strand
	// that entry until the next mutation (and lose it entirely if it was the
	// last one before shutdown). R20260610-085718-LB-6 (#2011).
	if rs.gen == savedGen {
		rs.dirty = false
	}
	rs.mu.Unlock()
	return nil
}

// Prune drops entries older than `cutoff` (unix ms). Returns the number of
// entries removed. Marks dirty when entries were dropped. Pair with a
// max-entry cap to defend against pathological growth: when the surviving
// set is still over rs.maxEntries, the oldest survivors are also dropped.
func (rs *RetiredStore) Prune(cutoffMs int64) int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	removed := 0
	for k, v := range rs.entries {
		if v < cutoffMs {
			delete(rs.entries, k)
			removed++
		}
	}
	if rs.maxEntries > 0 && len(rs.entries) > rs.maxEntries {
		type kv struct {
			id string
			ts int64
		}
		// O(N log N) trim — Prune is called on a slow ticker (≥1m) so the
		// extra sort is negligible vs. the I/O the missed prune would
		// cost. Sort ascending by ts; drop the prefix that exceeds the cap.
		all := make([]kv, 0, len(rs.entries))
		for k, v := range rs.entries {
			all = append(all, kv{k, v})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].ts < all[j].ts })
		excess := len(all) - rs.maxEntries
		for i := 0; i < excess; i++ {
			delete(rs.entries, all[i].id)
			removed++
		}
	}
	if removed > 0 {
		rs.dirty = true
		rs.gen++
	}
	return removed
}

// Len returns the current entry count. Primarily for tests and metrics.
func (rs *RetiredStore) Len() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.entries)
}
