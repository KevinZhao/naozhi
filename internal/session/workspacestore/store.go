// Package workspacestore holds the per-chat workspace-override state owned by
// session.Router (the `wsStore` facet, Router P1 #383, encapsulated for
// #2495 step 1).
//
// Moving the facet out of package session makes every inner field private to
// THIS package, so the compiler — not the `// 读写:` annotation lint
// (tools/check-router-fields) — enforces that Router code goes through the
// method surface below. Router keeps the outer `wsStore` field annotation;
// the per-inner-field annotations that used to live on workspaceStore are
// gone because no other package can reach the fields.
//
// # Lock contract
//
// Store carries NO lock of its own and is NOT safe for concurrent use.
// Every method must be called with session.Router.mu held: the read-only
// methods (Lookup, Len, Range, Snapshot, Dirty, Gen, CheckInvariants) are
// safe under RLock; every other method requires the exclusive Lock.
//
// Why the store does not own its lock (decision recorded in the #2495 PR):
// workspace-override mutations are required to be atomic WITH session
// mutations in two documented places — ResetChatAndSetWorkspace (#2342:
// a concurrent reader must never observe "chat reset but override gone")
// and ResetAndDiscardOverride (Round-207 SM1: a concurrent SetWorkspace
// must not survive a Reset+delete pair) — and SetBounded's eviction
// consults the live-session index (r.ss.byChat). Those cross-facet
// invariants only hold if the override state and the session state are
// guarded by the same mutex, so the store stays under r.mu.
//
// # Two-key invariant (carried over verbatim from workspaceStore)
//
// Keys are 3-segment chat keys "platform:chatType:chatID" — distinct from
// the 4-segment session key used in Router's session index. Every chatKey
// present in Router's byChat index may have an override entry; ResetChat
// clears both. SetWorkspace creates only the override entry (no session
// yet), and Reset(key)/evictOldest must NOT touch this store — it is driven
// by user intent (SetWorkspace) rather than the session lifecycle.
package workspacestore

import (
	"fmt"
	"log/slog"
	"sync/atomic"
)

// Store is the per-chat workspace-override container. The zero value is
// ready to use (maps are allocated lazily on first write), so a bare
// &session.Router{} in tests needs no explicit initialisation.
//
// Store must not be copied after first use (gen is an atomic.Uint64); it is
// embedded by value in session.Router, which is always heap-allocated.
type Store struct {
	// overrides maps chatKey → workspace path.
	overrides map[string]string
	// seq records the Set insertion order per chatKey, used as the LRU
	// recency signal for capacity self-healing (#cwd-overflow). A key with
	// NO seq entry (Seed-loaded from disk on restart, or Adopt-installed via
	// Takeover) is treated as oldest — exactly the eviction priority we
	// want, since the stale historical one-shot keys that fill the map are
	// precisely those loaded from disk. Maintained only by Set; cleared by
	// Delete and pruned during eviction so it cannot outgrow overrides.
	seq map[string]uint64
	// seqNext is the monotonic counter handed to the next Set write.
	seqNext uint64
	// dirty is true when overrides changed since the last successful save.
	dirty bool
	// gen increments on each override mutation; the save path snapshots it
	// before releasing the lock and only clears dirty if it is unchanged
	// afterwards (see MarkSavedIfUnchanged). Kept atomic for parity with the
	// pre-extraction field; there is no lock-free reader today.
	gen atomic.Uint64
}

// Lookup returns the override for chatKey and whether one exists.
func (s *Store) Lookup(chatKey string) (string, bool) {
	ws, ok := s.overrides[chatKey]
	return ws, ok
}

// Len returns the number of overrides.
func (s *Store) Len() int { return len(s.overrides) }

// Range calls fn for every (chatKey, path) pair in unspecified order.
// fn must not mutate the store.
func (s *Store) Range(fn func(chatKey, path string)) {
	for k, v := range s.overrides {
		fn(k, v)
	}
}

// Snapshot returns a copy of the overrides. The result is never nil — an
// empty store yields an empty (non-nil) map — because the save path uses
// "copy != nil" as its "a save is due" signal and an empty map must still
// be persisted to record the deletion of the last override.
func (s *Store) Snapshot() map[string]string {
	out := make(map[string]string, len(s.overrides))
	for k, v := range s.overrides {
		out[k] = v
	}
	return out
}

// Seed installs entries loaded from persistent storage without stamping a
// seq (they sort oldest for eviction), without marking the store dirty and
// without bumping gen — a fresh load must not trigger an immediate re-save.
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
// It performs NO capacity check — callers that must honour the override cap
// use SetBounded. Existing keys are updated in place and re-stamped as
// most-recently-set.
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

// SetBounded is Set with the capacity policy applied (R58-SEC-H1 DoS bound
// + #cwd-overflow self-healing): an existing key updates in place (no
// growth, no eviction); a brand-new key at capacity first evicts the
// least-recently-set override whose chat isLive reports false; only if
// nothing is evictable is the new write dropped (every remaining override
// belongs to a live chat — a genuine over-cap fleet, the DoS case the bound
// is for). Reports whether the override was written.
//
// isLive is invoked synchronously, under the caller's lock, once per
// override during the rare at-capacity scan (O(n) over a bounded map).
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

// Adopt installs an override WITHOUT an LRU seq stamp (the entry sorts
// oldest for eviction, like a disk-loaded one) and marks the store dirty
// only when the value actually changed. Used by Takeover, which must
// persist its chosen workspace but must not re-dirty the store when the
// prior value already matches. Reports whether anything changed.
func (s *Store) Adopt(chatKey, path string) bool {
	if prev, ok := s.overrides[chatKey]; ok && prev == path {
		return false
	}
	s.ensureOverrides()
	s.overrides[chatKey] = path
	s.markMutated()
	return true
}

// Delete removes the override (and its seq stamp) for chatKey and marks
// the store dirty. Without the dirty bump the delete would only be written
// back when some other path flips the flag; a crash before that would
// reload the override on restart and silently undo the user's reset.
// Reports whether an override existed.
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

// MarkSavedIfUnchanged clears the dirty flag iff the mutation generation
// still equals snapshotGen (the value Gen returned when the caller took the
// Snapshot it just persisted). A concurrent mutation between snapshot and
// save leaves dirty set so the next tick re-persists. Reports whether dirty
// was cleared.
func (s *Store) MarkSavedIfUnchanged(snapshotGen uint64) bool {
	if s.gen.Load() != snapshotGen {
		return false
	}
	s.dirty = false
	return true
}

// CheckInvariants reports the first violated internal invariant, or nil.
// Currently: every seq stamp must belong to a present override (seq never
// outgrows overrides). Intended for tests and diagnostics.
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
// reports false, returning the victim key. ok is false when every override
// belongs to a live chat (nothing safe to evict).
//
// Recency = seq (Set insertion order); a key absent from seq (Seed / Adopt
// installed) sorts as oldest, which is the desired priority — the stale
// one-shot keys that overflow the map are exactly those.
func (s *Store) evictLRU(isLive func(chatKey string) bool) (victim string, ok bool) {
	const noSeq = uint64(0) // keys without a seq entry sort oldest
	var victimSeq uint64
	for k := range s.overrides {
		if isLive != nil && isLive(k) {
			continue // live session — never evict
		}
		seq := noSeq
		if s.seq != nil {
			seq = s.seq[k] // 0 when absent → oldest
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
