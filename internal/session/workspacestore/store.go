// Package workspacestore holds the per-chat workspace-override state owned by
// session.Router (the `wsStore` facet); fields are private so the compiler
// enforces access through the method surface (#2495).
//
// Lock contract: Store carries NO lock. Call every method with
// session.Router.mu held — Lookup, Len, Range, Snapshot, Dirty, Gen and
// CheckInvariants under RLock, everything else under Lock. Override mutations
// must be atomic with session mutations (ResetChatAndSetWorkspace,
// ResetAndDiscardOverride) and SetBounded's eviction reads the live-session
// index, so both share r.mu.
//
// Two-key invariant: keys are 3-segment chat keys "platform:chatType:chatID",
// not the 4-segment session key. ResetChat clears both; SetWorkspace creates
// only the override; Reset(key) and evictOldest must NOT touch this store.
package workspacestore

import (
	"fmt"
	"log/slog"
	"sync/atomic"
)

// Store is the per-chat workspace-override container; the zero value is ready
// to use. Must not be copied after first use (gen is atomic).
type Store struct {
	// overrides maps chatKey → workspace path.
	overrides map[string]string
	// seq records Set insertion order per chatKey — the LRU signal for
	// capacity self-healing. A key with NO seq entry (Seed / Adopt installed)
	// is oldest, which is the desired eviction priority.
	seq map[string]uint64
	// seqNext is the monotonic counter handed to the next Set write.
	seqNext uint64
	// dirty is true when overrides changed since the last successful save.
	dirty bool
	// gen increments on each mutation; the save path compares it after the
	// write to decide whether dirty may be cleared (MarkSavedIfUnchanged).
	gen atomic.Uint64
}

// Lookup returns the override for chatKey and whether one exists.
func (s *Store) Lookup(chatKey string) (string, bool) {
	ws, ok := s.overrides[chatKey]
	return ws, ok
}

// Len returns the number of overrides.
func (s *Store) Len() int { return len(s.overrides) }

// Range calls fn for every (chatKey, path) pair; fn must not mutate the store.
func (s *Store) Range(fn func(chatKey, path string)) {
	for k, v := range s.overrides {
		fn(k, v)
	}
}

// Snapshot returns a copy of the overrides, never nil: the save path uses
// "copy != nil" as its signal and must persist the last deletion.
func (s *Store) Snapshot() map[string]string {
	out := make(map[string]string, len(s.overrides))
	for k, v := range s.overrides {
		out[k] = v
	}
	return out
}

// Seed installs disk-loaded entries without a seq stamp (oldest for
// eviction), without dirtying and without bumping gen.
func (s *Store) Seed(entries map[string]string) {
	if len(entries) == 0 {
		return
	}
	s.ensureOverrides()
	for k, v := range entries {
		s.overrides[k] = v
	}
}

// Set writes the override plus its LRU seq stamp and marks the store dirty.
// No capacity check — callers honouring the override cap use SetBounded.
func (s *Store) Set(chatKey, path string) {
	s.ensureOverrides()
	if s.seq == nil {
		s.seq = make(map[string]uint64)
	}
	s.overrides[chatKey] = path
	s.seqNext++
	s.seq[chatKey] = s.seqNext
	s.markMutated()
}

// SetBounded is Set with the capacity policy: an existing key updates in
// place; a new key at capacity first evicts the least-recently-set override
// whose chat isLive reports false; if nothing is evictable the write is
// dropped (every override belongs to a live chat — the DoS case the bound
// is for). isLive runs under the caller's lock, once per override at capacity.
func (s *Store) SetBounded(chatKey, path string, capacity int, isLive func(chatKey string) bool) bool {
	if _, existing := s.overrides[chatKey]; !existing && len(s.overrides) >= capacity {
		victim, ok := s.evictLRU(isLive)
		if !ok {
			slog.Warn("workspaceOverrides at capacity and no session-less entry to evict; dropping override",
				"chat_key", chatKey, "cap", capacity)
			return false
		}
		slog.Info("workspaceOverrides at capacity; evicted least-recently-set session-less override",
			"evicted_chat_key", victim, "cap", capacity)
	}
	s.Set(chatKey, path)
	return true
}

// Adopt installs an override WITHOUT a seq stamp (oldest for eviction) and
// dirties only when the value changed (Takeover must not re-dirty a match).
func (s *Store) Adopt(chatKey, path string) bool {
	if prev, ok := s.overrides[chatKey]; ok && prev == path {
		return false
	}
	s.ensureOverrides()
	s.overrides[chatKey] = path
	s.markMutated()
	return true
}

// Delete removes the override and its seq stamp and marks the store dirty,
// so a crash before the next save cannot resurrect the user's reset.
func (s *Store) Delete(chatKey string) bool {
	if _, existed := s.overrides[chatKey]; !existed {
		return false
	}
	delete(s.overrides, chatKey)
	delete(s.seq, chatKey)
	s.markMutated()
	return true
}

// Dirty reports whether overrides changed since the last successful save.
func (s *Store) Dirty() bool { return s.dirty }

// Gen returns the current mutation generation.
func (s *Store) Gen() uint64 { return s.gen.Load() }

// MarkSavedIfUnchanged clears dirty iff gen still equals snapshotGen (the
// Gen value taken with the persisted Snapshot); a concurrent mutation leaves
// dirty set so the next tick re-persists.
func (s *Store) MarkSavedIfUnchanged(snapshotGen uint64) bool {
	if s.gen.Load() != snapshotGen {
		return false
	}
	s.dirty = false
	return true
}

// CheckInvariants reports the first violated internal invariant (every seq
// stamp must belong to a present override), or nil.
func (s *Store) CheckInvariants() error {
	for k := range s.seq {
		if _, ok := s.overrides[k]; !ok {
			return fmt.Errorf("workspacestore: seq stamp for %q has no override (seq=%d overrides=%d)",
				k, len(s.seq), len(s.overrides))
		}
	}
	return nil
}

// evictLRU removes the least-recently-set override whose chat isLive
// reports false; ok is false when every override belongs to a live chat.
// Keys absent from seq (Seed / Adopt installed) sort oldest.
func (s *Store) evictLRU(isLive func(chatKey string) bool) (victim string, ok bool) {
	const noSeq = uint64(0)
	var victimSeq uint64
	for k := range s.overrides {
		if isLive != nil && isLive(k) {
			continue
		}
		seq := noSeq
		if s.seq != nil {
			seq = s.seq[k]
		}
		if !ok || seq < victimSeq {
			victim, victimSeq, ok = k, seq, true
		}
	}
	if !ok {
		return "", false
	}
	delete(s.overrides, victim)
	delete(s.seq, victim)
	return victim, true
}

func (s *Store) ensureOverrides() {
	if s.overrides == nil {
		s.overrides = make(map[string]string)
	}
}

func (s *Store) markMutated() {
	s.dirty = true
	s.gen.Add(1)
}
