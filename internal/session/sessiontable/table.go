// Package sessiontable is session.Router's session table: key → session,
// the indices kept alongside it (chat → keys, key-hash → key, session ID →
// key), the live-process count and the change generation. Fields are private,
// so every mutation goes through a method that keeps the indices in step with
// the table; the invariants that used to be comments on Router are enforced
// here.
//
// Table owns its lock, and the only way at the table is a transaction: View
// (read lock) or Update (write lock), plus the single-value reads Load and
// Count and the lock-free atomics Active, Gen and BumpGen. The lock also
// guards the state the caller keeps beside the table and changes atomically
// with it (spawn bookkeeping, workspace overrides, picks), reached through
// the transaction's Ext.
//
// S is the session type and X the caller's state kept under the same lock;
// R is the read-only view of X a read transaction gets. The table looks
// inside none of them. Every index key is derived from the session
// key string, or handed in (the session ID), so no constraint is needed.
package sessiontable

import (
	"fmt"
	"iter"
	"sort"
	"sync"
	"sync/atomic"
)

// Table is the session table. The zero value is not usable; use New.
type Table[S, X, R any] struct {
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
	// is published, so it is maintained by setID / clearID rather than put.
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

	// ext is the caller's state that changes atomically with the table and
	// is guarded by the same lock; the table never looks inside it.
	ext X
	// readOnly turns ext into R, the view a read transaction gets: a View
	// holds only the read lock, so it must not be able to write ext.
	readOnly func(*X) R

	// seqs numbers every Tx issued; liveSeq is the number of the Tx that
	// currently holds the write lock (0 when none does), so a Tx used after
	// its Update, or while Unlocked, is caught.
	seqs    uint64
	liveSeq uint64
}

// New returns an empty table with a zero X. chatOf maps a session key to its chat key;
// hashOf maps it to the hash keyForHash resolves; readOnly gives a read
// transaction its view R of the caller's state.
func New[S, X, R any](chatOf, hashOf func(key string) string, readOnly func(*X) R) *Table[S, X, R] {
	t := &Table[S, X, R]{
		readOnly: readOnly,
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

// get returns key's session, or the zero S when there is none.
func (t *Table[S, X, R]) get(key string) S {
	return t.sessions[key]
}

// lookup returns key's session and whether there is one.
func (t *Table[S, X, R]) lookup(key string) (S, bool) {
	s, ok := t.sessions[key]
	return s, ok
}

// size is the number of sessions.
func (t *Table[S, X, R]) size() int { return len(t.sessions) }

// all iterates the sessions in unspecified order. The table must not be
// mutated during the iteration; collect keys first when it will be.
func (t *Table[S, X, R]) all() iter.Seq2[string, S] {
	return func(yield func(string, S) bool) {
		for k, s := range t.sessions {
			if !yield(k, s) {
				return
			}
		}
	}
}

// put installs s for key, indexing its chat and hash. An existing session
// for key is replaced; its session-ID mapping is the caller's to retire.
func (t *Table[S, X, R]) put(key string, s S) {
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

// remove removes key's session and its chat and hash entries. The session-ID
// mapping is the caller's to retire (clearID / clearIDIfOwnedBy), because only
// the caller knows the session's ID.
func (t *Table[S, X, R]) remove(key string) {
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

// keysOfChat returns a copy of the session keys under chat, so the caller may
// remove them while walking the result.
func (t *Table[S, X, R]) keysOfChat(chat string) []string {
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

// chatHasSessions reports whether any session key maps to chat.
func (t *Table[S, X, R]) chatHasSessions(chat string) bool {
	return len(t.byChat[chat]) > 0
}

// keyForHash resolves a key hash to its session key.
func (t *Table[S, X, R]) keyForHash(hash string) (string, bool) {
	k, ok := t.keyhash[hash]
	return k, ok
}

// setID points session ID id at key; an empty id is ignored.
func (t *Table[S, X, R]) setID(id, key string) {
	if id == "" {
		return
	}
	t.idToKey[id] = key
}

// clearID drops id's mapping.
func (t *Table[S, X, R]) clearID(id string) {
	delete(t.idToKey, id)
}

// clearIDIfOwnedBy drops id's mapping only while it still points at key, so
// a key reused by an unrelated session keeps its mapping when the previous
// owner is cleaned up.
func (t *Table[S, X, R]) clearIDIfOwnedBy(id, key string) {
	if mapped, ok := t.idToKey[id]; ok && mapped == key {
		delete(t.idToKey, id)
	}
}

// keyForID resolves a session ID to its session key.
func (t *Table[S, X, R]) keyForID(id string) (string, bool) {
	k, ok := t.idToKey[id]
	return k, ok
}

// Active is the live-process count. Safe without the lock.
func (t *Table[S, X, R]) Active() int64 { return t.active.Load() }

// addActive adjusts the live-process count and returns the new value.
func (t *Table[S, X, R]) addActive(n int64) int64 { return t.active.Add(n) }

// setActive replaces the live-process count, after a recount.
func (t *Table[S, X, R]) setActive(n int64) { t.active.Store(n) }

// Gen is the change generation. Safe without the lock.
func (t *Table[S, X, R]) Gen() uint64 { return t.gen.Load() }

// BumpGen advances the change generation without marking the table for save:
// a change pollers must see that is not persisted.
func (t *Table[S, X, R]) BumpGen() { t.gen.Add(1) }

// markChanged marks the table for save and advances the change generation.
func (t *Table[S, X, R]) markChanged() {
	t.dirty = true
	t.gen.Add(1)
}

// isDirty reports whether the table changed since the save flag was cleared.
func (t *Table[S, X, R]) isDirty() bool { return t.dirty }

// setDirty sets the save flag without advancing the generation.
func (t *Table[S, X, R]) setDirty(v bool) { t.dirty = v }

// check returns every disagreement between the table and its indices, sorted;
// empty when they are consistent. idOf reports a session's ID ("" when it has
// none yet), for the idToKey direction the caller maintains.
func (t *Table[S, X, R]) check(idOf func(S) string) []string {
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
