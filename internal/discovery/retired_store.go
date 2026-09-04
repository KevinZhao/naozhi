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
// (Router.Reset / Router.Remove → notifyKeyRetired) so the dashboard history
// drawer can sort by it rather than by JSONL mtime. Persisted as one JSON
// file alongside sessions.json; best-effort — a corrupt/missing file degrades
// to LastActive ordering. All public methods take mu; MarkRetired does no I/O
// and Save runs from a periodic flusher, so disk may lag memory (a UX hint).
type RetiredStore struct {
	path string

	mu      sync.Mutex
	entries map[string]int64 // sessionID → unix ms
	dirty   bool

	// gen counts mutations. Save snapshots it with the entries copy and only
	// clears dirty afterwards if gen is unchanged; otherwise a MarkRetired
	// landing during the unlocked write would have dirty cleared for an entry
	// the write did not include, losing it across a restart (#2011).
	gen uint64

	// maxEntries caps how many sessionIDs survive Prune; RetiredAt may post-date
	// the JSONL mtime by weeks, so the store has no natural expiry. Default
	// 4096 (~80 KB) far exceeds a busy operator's ~50 closed sessions per week.
	maxEntries int
}

// retiredStoreFileV1 is the on-disk schema; Version is reserved for future
// migrations and readers tolerate unknown fields.
type retiredStoreFileV1 struct {
	Version int              `json:"version"`
	Entries map[string]int64 `json:"entries"`
}

const retiredStoreVersion = 1

// DefaultRetiredStoreMaxEntries is the cap used when callers don't override it
// via NewRetiredStoreWithCap.
const DefaultRetiredStoreMaxEntries = 4096

// NewRetiredStore constructs a store backed by `path` (empty = in-memory only);
// the first Save() creates the file. Load errors are returned but do not block
// construction, since RetiredAt is purely a UX sort hint.
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
// The most recent timestamp wins, so a Reset → Remove sequence reports the
// Remove instant. An empty sessionID (resume failed before init) is ignored.
// Marks dirty for the next Save(); does no I/O.
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

// Save writes the current map atomically to disk; a no-op (nil) when the path
// is empty or nothing changed since the last successful Save. Called from a
// periodic ticker and once at shutdown.
func (rs *RetiredStore) Save() error {
	rs.mu.Lock()
	if rs.path == "" || !rs.dirty {
		rs.mu.Unlock()
		return nil
	}
	// Snapshot under lock so the encode runs on a stable copy; gen detects
	// mutations that land during the unlocked write window (#2011).
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
	if err := osutil.WriteFileAtomic(rs.path, data, 0o600); err != nil {
		return fmt.Errorf("write retired store: %w", err)
	}

	rs.mu.Lock()
	// Only clear dirty if no mutation landed during the unlocked write (#2011).
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
		// Prune runs on a slow (≥1m) ticker, so an O(N log N) sort is fine.
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
