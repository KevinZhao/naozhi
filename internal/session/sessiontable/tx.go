package sessiontable

import "iter"

// View is a read transaction: the table's read lock is held while it is in
// use. It is a value handed to View's callback; using one after the callback
// returns is a data race (-race reports it), since the lock is gone.
type View[S, X, R any] struct {
	t *Table[S, X, R]
}

// Tx is a write transaction: the table's write lock is held while it is in
// use, except inside Wait and Unlocked, which release it and take it again.
// It is a value handed to Update's callback; its mutators panic when called
// after the callback returns, or from inside Unlocked.
type Tx[S, X, R any] struct {
	View[S, X, R]
	seq uint64
}

// View runs fn with the read lock held. The lock is released however fn
// returns, panics included.
func (t *Table[S, X, R]) View(fn func(v View[S, X, R])) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	fn(View[S, X, R]{t: t})
}

// Update runs fn with the write lock held. The lock is released however fn
// returns, panics included.
func (t *Table[S, X, R]) Update(fn func(tx Tx[S, X, R])) {
	t.mu.Lock()
	defer t.end()
	fn(t.begin())
}

// end retires the live Tx and releases the write lock.
func (t *Table[S, X, R]) end() {
	t.liveSeq = 0
	t.mu.Unlock()
}

// begin issues a Tx for the current write-lock holder.
func (t *Table[S, X, R]) begin() Tx[S, X, R] {
	t.seqs++
	t.liveSeq = t.seqs
	return Tx[S, X, R]{View: View[S, X, R]{t: t}, seq: t.seqs}
}

// live returns the table when tx still holds the write lock.
func (tx Tx[S, X, R]) live() *Table[S, X, R] {
	if tx.t.liveSeq != tx.seq {
		panic("sessiontable: Tx used outside the Update that issued it")
	}
	return tx.t
}

func (v View[S, X, R]) Get(key string) S                   { return v.t.get(key) }
func (v View[S, X, R]) Lookup(key string) (S, bool)        { return v.t.lookup(key) }
func (v View[S, X, R]) Len() int                           { return v.t.size() }
func (v View[S, X, R]) All() iter.Seq2[string, S]          { return v.t.all() }
func (v View[S, X, R]) KeysOfChat(chat string) []string    { return v.t.keysOfChat(chat) }
func (v View[S, X, R]) ChatHasSessions(chat string) bool   { return v.t.chatHasSessions(chat) }
func (v View[S, X, R]) KeyForHash(h string) (string, bool) { return v.t.keyForHash(h) }
func (v View[S, X, R]) KeyForID(id string) (string, bool)  { return v.t.keyForID(id) }
func (v View[S, X, R]) Active() int64                      { return v.t.Active() }
func (v View[S, X, R]) Gen() uint64                        { return v.t.Gen() }
func (v View[S, X, R]) Dirty() bool                        { return v.t.isDirty() }
func (v View[S, X, R]) Check(idOf func(S) string) []string { return v.t.check(idOf) }

// Ext is the read-only view of the caller's state kept under the table's
// lock: a View holds only the read lock, so it gets R, not *X.
func (v View[S, X, R]) Ext() R { return v.t.readOnly(&v.t.ext) }

// Ext is the caller's state kept under the table's lock, for writing.
func (tx Tx[S, X, R]) Ext() *X { return &tx.live().ext }

func (tx Tx[S, X, R]) Put(key string, s S)             { tx.live().put(key, s) }
func (tx Tx[S, X, R]) Delete(key string)               { tx.live().remove(key) }
func (tx Tx[S, X, R]) SetID(id, key string)            { tx.live().setID(id, key) }
func (tx Tx[S, X, R]) ClearID(id string)               { tx.live().clearID(id) }
func (tx Tx[S, X, R]) ClearIDIfOwnedBy(id, key string) { tx.live().clearIDIfOwnedBy(id, key) }
func (tx Tx[S, X, R]) AddActive(n int64) int64         { return tx.live().addActive(n) }
func (tx Tx[S, X, R]) SetActive(n int64)               { tx.live().setActive(n) }
func (tx Tx[S, X, R]) BumpGen()                        { tx.live().BumpGen() }
func (tx Tx[S, X, R]) MarkChanged()                    { tx.live().markChanged() }
func (tx Tx[S, X, R]) SetDirty(d bool)                 { tx.live().setDirty(d) }

// Broadcast wakes every Wait.
func (tx Tx[S, X, R]) Broadcast() { tx.live().cond.Broadcast() }

// Wait releases the lock until Broadcast, then takes it again. Everything
// read before Wait may have changed after it; call it in a loop re-checking
// the condition waited for.
func (tx Tx[S, X, R]) Wait() {
	t := tx.live()
	t.liveSeq = 0
	t.cond.Wait()
	t.liveSeq = tx.seq
}

// Unlocked runs fn with the lock released and takes it again afterwards, for
// slow work (closing a process, copying history) inside a transaction.
// Everything read before Unlocked may have changed after it, and tx's
// mutators panic inside fn.
func (tx Tx[S, X, R]) Unlocked(fn func()) {
	t := tx.live()
	t.liveSeq = 0
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.liveSeq = tx.seq
	}()
	fn()
}

// Load returns key's session (zero S when none) under the read lock, for a
// caller that needs that one value and nothing else consistent with it.
func (t *Table[S, X, R]) Load(key string) S {
	t.mu.RLock()
	s := t.sessions[key]
	t.mu.RUnlock()
	return s
}

// Count returns the number of sessions and the live-process count, read
// together under the read lock.
func (t *Table[S, X, R]) Count() (sessions int, active int64) {
	t.mu.RLock()
	sessions, active = len(t.sessions), t.active.Load()
	t.mu.RUnlock()
	return sessions, active
}

// Healthy reports whether the read lock can be taken right now, for a
// liveness probe that must not block behind a stuck holder.
func (t *Table[S, X, R]) Healthy() bool {
	if !t.mu.TryRLock() {
		return false
	}
	t.mu.RUnlock()
	return true
}
