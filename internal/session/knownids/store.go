// Package knownids holds every session ID naozhi has ever used (the
// session.Router `kid` facet), which discovery uses to tell naozhi-owned CLI
// processes from external ones. Fields are private so the compiler enforces
// access through the method surface (#2495).
//
// Lock contract: Store owns its mutex and no method requires
// session.Router.mu. The facet needs no cross-facet atomicity, so it is off
// r.mu entirely. Track is still invoked with r.mu held at the publish sites,
// which fixes the lock order r.mu → Store.mu; Store never calls back into
// Router or any caller code, so the reverse edge cannot form.
//
// Save protocol (Cleanup / saveIfDirty): ClaimSave atomically checks dirty +
// throttle, stamps savedAt and returns gen-memoised JSON; the caller writes
// the file outside every lock, then calls MarkSavedIfUnchanged(gen) on
// success or ResetSaveThrottle on failure. MarshalSnapshot is the
// unthrottled variant for Shutdown.
package knownids

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"
)

// MaxKnownIDs caps the set; overflow evicts FIFO by insertion order so a
// still-active ID is never dropped ahead of older ones. UUIDs are 36 bytes,
// so 10K entries cost ~360KB in memory.
const MaxKnownIDs = 10000

// marshalJSON is swapped by tests to exercise the marshal-error path.
var marshalJSON = json.Marshal

// Store is the known-session-ID container; the zero value is ready to use
// and must not be copied after first use.
type Store struct {
	mu sync.Mutex
	// ids holds every tracked ID, including removed/reset/evicted sessions.
	ids map[string]bool
	// order is the insertion sequence. The live window is order[orderHead:]
	// and mirrors the keys of ids; slots before orderHead are evicted and
	// cleared, and the dead prefix is compacted once it reaches half the slice.
	order     []string
	orderHead int
	// dirty is true when ids changed since the last successful save.
	dirty bool
	// gen increments on every ids mutation and gates both memo caches.
	gen uint64
	// sortedCache / marshaledCache memoise the sorted IDs and their JSON at
	// sortedGen / marshaledGen (0 = unbuilt), so an unchanged set costs one
	// sort and one marshal per mutation generation, not per save tick.
	sortedCache    []string
	sortedGen      uint64
	marshaledCache []byte
	marshaledGen   uint64
	// savedAt is the last ClaimSave stamp; zero means the throttle is open.
	savedAt time.Time
}

// Seed installs disk-loaded IDs without dirtying or bumping gen. The file
// carries no insertion order, so order is seeded from map iteration and FIFO
// eviction resumes from that arbitrary order after a restart.
func (s *Store) Seed(ids map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureIDs()
	for id := range ids {
		if s.ids[id] {
			continue
		}
		s.ids[id] = true
		s.order = append(s.order, id)
	}
}

// Track adds id to the set and reports whether it was new; an empty id is
// ignored. At MaxKnownIDs the oldest entry is evicted first.
func (s *Store) Track(id string) bool {
	if id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids[id] {
		return false
	}
	s.ensureIDs()
	if len(s.ids) >= MaxKnownIDs {
		s.evictOldestLocked()
	}
	s.ids[id] = true
	s.order = append(s.order, id)
	s.gen++
	s.dirty = true
	return true
}

// Has reports whether id has been tracked.
func (s *Store) Has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ids[id]
}

// Len returns the number of tracked IDs.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ids)
}

// SortedSnapshot returns the IDs sorted ascending as a fresh copy the caller
// may keep after the lock is released. The sort is memoised by gen.
func (s *Store) SortedSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sortedLocked())
}

// MarshalSnapshot returns the sorted set as JSON regardless of dirty or
// throttle state (Shutdown's final save). The bytes are a fresh copy.
func (s *Store) MarshalSnapshot() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.marshaledLocked()
}

// ClaimSave claims the periodic save window: when the set is dirty and at
// least interval has elapsed since the previous claim it stamps savedAt=now
// and returns the memoised JSON plus the gen it reflects, with due=true.
// The caller writes the bytes outside every lock, then calls
// MarkSavedIfUnchanged(gen) on success or ResetSaveThrottle on failure. A
// marshal error releases the claim so the next tick retries.
func (s *Store) ClaimSave(now time.Time, interval time.Duration) (data []byte, gen uint64, due bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty || now.Sub(s.savedAt) < interval {
		return nil, 0, false, nil
	}
	s.savedAt = now
	data, err = s.marshaledLocked()
	if err != nil {
		s.savedAt = time.Time{}
		return nil, 0, false, err
	}
	return data, s.gen, true, nil
}

// MarkSavedIfUnchanged clears dirty iff gen still equals snapshotGen (the
// value returned by ClaimSave); a Track since the claim leaves dirty set so
// the next tick re-persists. An add + evict pair keeps Len identical, which
// is why gen rather than length is compared.
func (s *Store) MarkSavedIfUnchanged(snapshotGen uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen != snapshotGen {
		return false
	}
	s.dirty = false
	return true
}

// ResetSaveThrottle reopens the throttle after a failed write so the next
// tick retries instead of waiting out the interval; dirty is left as is.
func (s *Store) ResetSaveThrottle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.savedAt = time.Time{}
}

// Dirty reports whether ids changed since the last successful save.
func (s *Store) Dirty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dirty
}

// Gen returns the current mutation generation.
func (s *Store) Gen() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}

// SavedAt returns the last ClaimSave stamp (zero when the throttle is open).
func (s *Store) SavedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.savedAt
}

// evictOldestLocked clears the front live slot and advances orderHead
// (amortized O(1)). Once the dead prefix reaches half the slice the live
// window is copied into a fresh buffer: reslicing would pin the old backing
// array and keep the evicted strings alive.
func (s *Store) evictOldestLocked() {
	oldest := s.order[s.orderHead]
	delete(s.ids, oldest)
	s.order[s.orderHead] = ""
	s.orderHead++
	if s.orderHead >= len(s.order)/2 {
		live := s.order[s.orderHead:]
		compacted := make([]string, len(live), MaxKnownIDs+1)
		copy(compacted, live)
		s.order = compacted
		s.orderHead = 0
	}
}

// sortedLocked returns the sorted cache, rebuilding it when gen moved on.
// Callers must clone before releasing the lock.
func (s *Store) sortedLocked() []string {
	if s.sortedCache == nil || s.sortedGen != s.gen {
		sorted := make([]string, 0, len(s.ids))
		for id := range s.ids {
			sorted = append(sorted, id)
		}
		slices.Sort(sorted)
		s.sortedCache = sorted
		s.sortedGen = s.gen
	}
	return s.sortedCache
}

// marshaledLocked returns a copy of the JSON cache, rebuilding it (and the
// sorted cache it derives from) when gen moved on.
func (s *Store) marshaledLocked() ([]byte, error) {
	if s.marshaledCache == nil || s.marshaledGen != s.gen {
		data, err := marshalJSON(s.sortedLocked())
		if err != nil {
			return nil, fmt.Errorf("marshal known IDs: %w", err)
		}
		s.marshaledCache = data
		s.marshaledGen = s.gen
	}
	return slices.Clone(s.marshaledCache), nil
}

func (s *Store) ensureIDs() {
	if s.ids == nil {
		s.ids = make(map[string]bool)
	}
}
