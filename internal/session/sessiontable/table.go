// Package sessiontable is session.Router's session table: key → session,
// the indices kept alongside it (chat → keys, key-hash → key, session ID →
// key), the live-process count and the change generation. Fields are private,
// so every mutation goes through a method that keeps the indices in step with
// the table; the invariants that used to be comments on Router are enforced
// here.
//
// Lock contract: Table owns its lock. Call every other method with it held
// (Lock / RLock) — reads under the read lock, mutations under the write lock
// — except Active and Gen, which are atomic for lock-free readers. The lock
// also guards the state the caller keeps beside the table and changes
// atomically with it (spawn bookkeeping, workspace overrides, picks).
//
// S is the session type; the table never looks inside it. Every index key is
// derived from the session key string, or handed in (the session ID), so no
// constraint on S is needed.
package sessiontable

import (
	"fmt"
	"iter"
	"sort"
	"sync"
	"sync/atomic"
)

// Table is the session table. The zero value is not usable; use New.
type Table[S any] struct {
	mu sync.RWMutex
	// cond is signalled when a process changes state; its Locker is mu's
	// write side, so Wait must be called with Lock held.
	cond *sync.Cond

	sessions map[string]S
	// byChat: chat key → set of session keys, for O(k) chat resets.
	byChat map[string]map[string]struct{}
	// keyhash: hashOf(session key) → session key, an O(1) lookup for callers
	// that only hold the hash.
	keyhash map[string]string
	// idToKey: session ID → session key. The ID is learned after the session
	// is published, so it is maintained by SetID / ClearID rather than Put.
	idToKey map[string]string

	// active counts live, non-exempt processes (the caller decides what
	// counts); atomic so Stats reads it without the lock.
	active atomic.Int64
	// gen increments on every change a poller must see; atomic so Version
	// reads it without the lock.
	gen atomic.Uint64
	// dirty is true when the table changed since the last save.
	dirty bool

	chatOf func(key string) string
	hashOf func(key string) string
}

// New returns an empty table. chatOf maps a session key to its chat key;
// hashOf maps it to the hash KeyForHash resolves.
func New[S any](chatOf, hashOf func(key string) string) *Table[S] {
	t := &Table[S]{
		sessions: make(map[string]S),
		byChat:   make(map[string]map[string]struct{}),
		keyhash:  make(map[string]string),
		idToKey:  make(map[string]string),
		chatOf:   chatOf,
		hashOf:   hashOf,
	}
	t.cond = sync.NewCond(&t.mu)
	return t
}

// Lock, Unlock, RLock, RUnlock and TryLock take and release the table's lock.
func (t *Table[S]) Lock()         { t.mu.Lock() }
func (t *Table[S]) Unlock()       { t.mu.Unlock() }
func (t *Table[S]) RLock()        { t.mu.RLock() }
func (t *Table[S]) RUnlock()      { t.mu.RUnlock() }
func (t *Table[S]) TryLock() bool { return t.mu.TryLock() }

// TryRLock takes the read lock if it is free, without waiting.
func (t *Table[S]) TryRLock() bool { return t.mu.TryRLock() }

// Broadcast wakes every Wait. Call it with Lock held, so a waiter that has
// checked its condition but not yet parked cannot miss the signal.
func (t *Table[S]) Broadcast() { t.cond.Broadcast() }

// Wait releases the lock until Broadcast, then re-takes it. Call it with Lock
// held, in a loop re-checking the condition waited for.
func (t *Table[S]) Wait() { t.cond.Wait() }

// Get returns key's session, or the zero S when there is none.
func (t *Table[S]) Get(key string) S {
	return t.sessions[key]
}

// Lookup returns key's session and whether there is one.
func (t *Table[S]) Lookup(key string) (S, bool) {
	s, ok := t.sessions[key]
	return s, ok
}

// Len is the number of sessions.
func (t *Table[S]) Len() int { return len(t.sessions) }

// All iterates the sessions in unspecified order. The table must not be
// mutated during the iteration; collect keys first when it will be.
func (t *Table[S]) All() iter.Seq2[string, S] {
	return func(yield func(string, S) bool) {
		for k, s := range t.sessions {
			if !yield(k, s) {
				return
			}
		}
	}
}

// Put installs s for key, indexing its chat and hash. An existing session
// for key is replaced; its session-ID mapping is the caller's to retire.
func (t *Table[S]) Put(key string, s S) {
	t.sessions[key] = s
	t.keyhash[t.hashOf(key)] = key
	ck := t.chatOf(key)
	set := t.byChat[ck]
	if set == nil {
		set = make(map[string]struct{})
		t.byChat[ck] = set
	}
	set[key] = struct{}{}
}

// Delete removes key's session and its chat and hash entries. The session-ID
// mapping is the caller's to retire (ClearID / ClearIDIfOwnedBy), because only
// the caller knows the session's ID.
func (t *Table[S]) Delete(key string) {
	delete(t.sessions, key)
	// Equality-guarded so a hash collision cannot remove another key's entry.
	if kh := t.hashOf(key); t.keyhash[kh] == key {
		delete(t.keyhash, kh)
	}
	ck := t.chatOf(key)
	if set := t.byChat[ck]; set != nil {
		delete(set, key)
		if len(set) == 0 {
			delete(t.byChat, ck)
		}
	}
}

// KeysOfChat returns a copy of the session keys under chat, so the caller may
// Delete them while walking the result.
func (t *Table[S]) KeysOfChat(chat string) []string {
	set := t.byChat[chat]
	if len(set) == 0 {
		return nil
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	return keys
}

// ChatHasSessions reports whether any session key maps to chat.
func (t *Table[S]) ChatHasSessions(chat string) bool {
	return len(t.byChat[chat]) > 0
}

// KeyForHash resolves a key hash to its session key.
func (t *Table[S]) KeyForHash(hash string) (string, bool) {
	k, ok := t.keyhash[hash]
	return k, ok
}

// SetID points session ID id at key; an empty id is ignored.
func (t *Table[S]) SetID(id, key string) {
	if id == "" {
		return
	}
	t.idToKey[id] = key
}

// ClearID drops id's mapping.
func (t *Table[S]) ClearID(id string) {
	delete(t.idToKey, id)
}

// ClearIDIfOwnedBy drops id's mapping only while it still points at key, so
// a key reused by an unrelated session keeps its mapping when the previous
// owner is cleaned up.
func (t *Table[S]) ClearIDIfOwnedBy(id, key string) {
	if mapped, ok := t.idToKey[id]; ok && mapped == key {
		delete(t.idToKey, id)
	}
}

// KeyForID resolves a session ID to its session key.
func (t *Table[S]) KeyForID(id string) (string, bool) {
	k, ok := t.idToKey[id]
	return k, ok
}

// Active is the live-process count. Safe without the lock.
func (t *Table[S]) Active() int64 { return t.active.Load() }

// AddActive adjusts the live-process count and returns the new value.
func (t *Table[S]) AddActive(n int64) int64 { return t.active.Add(n) }

// SetActive replaces the live-process count, after a recount.
func (t *Table[S]) SetActive(n int64) { t.active.Store(n) }

// Gen is the change generation. Safe without the lock.
func (t *Table[S]) Gen() uint64 { return t.gen.Load() }

// BumpGen advances the change generation without marking the table for save:
// a change pollers must see that is not persisted.
func (t *Table[S]) BumpGen() { t.gen.Add(1) }

// MarkChanged marks the table for save and advances the change generation.
func (t *Table[S]) MarkChanged() {
	t.dirty = true
	t.gen.Add(1)
}

// Dirty reports whether the table changed since the save flag was cleared.
func (t *Table[S]) Dirty() bool { return t.dirty }

// SetDirty sets the save flag without advancing the generation.
func (t *Table[S]) SetDirty(v bool) { t.dirty = v }

// Check returns every disagreement between the table and its indices, sorted;
// empty when they are consistent. idOf reports a session's ID ("" when it has
// none yet), for the idToKey direction the caller maintains.
func (t *Table[S]) Check(idOf func(S) string) []string {
	var problems []string
	for key, s := range t.sessions {
		ck := t.chatOf(key)
		if _, ok := t.byChat[ck][key]; !ok {
			problems = append(problems, fmt.Sprintf("byChat[%q] is missing live session %q", ck, key))
		}
		if got := t.keyhash[t.hashOf(key)]; got != key {
			problems = append(problems, fmt.Sprintf("keyhash for %q = %q", key, got))
		}
		if id := idOf(s); id != "" {
			if got, ok := t.idToKey[id]; !ok {
				problems = append(problems, fmt.Sprintf("live session %q reports id %q with no idToKey entry", key, id))
			} else if got != key {
				problems = append(problems, fmt.Sprintf("idToKey[%q] = %q, but that id belongs to live session %q", id, got, key))
			}
		}
	}
	for ck, set := range t.byChat {
		if len(set) == 0 {
			problems = append(problems, fmt.Sprintf("byChat[%q] is an empty set", ck))
		}
		for key := range set {
			if _, ok := t.sessions[key]; !ok {
				problems = append(problems, fmt.Sprintf("byChat[%q] holds dead session %q", ck, key))
			}
			if got := t.chatOf(key); got != ck {
				problems = append(problems, fmt.Sprintf("byChat[%q] holds %q whose chat is %q", ck, key, got))
			}
		}
	}
	for kh, key := range t.keyhash {
		if _, ok := t.sessions[key]; !ok {
			problems = append(problems, fmt.Sprintf("keyhash[%q] holds dead session %q", kh, key))
		}
	}
	for id, key := range t.idToKey {
		if _, ok := t.sessions[key]; !ok {
			problems = append(problems, fmt.Sprintf("idToKey[%q] holds dead session %q", id, key))
		}
	}
	sort.Strings(problems)
	return problems
}
