package sessiontable

import "iter"

// View is a read transaction: the table's read lock is held while it is in
// use. It is a value handed to View's callback; using one after the callback
// returns is a data race (-race reports it), since the lock is gone.
type View[S, X any] struct {
	t *Table[S, X]
}

// Tx is a write transaction: the table's write lock is held while it is in
// use, except inside Wait and Unlocked, which release it and take it again.
// It is a value handed to Update's callback; its mutators panic when called
// after the callback returns, or from inside Unlocked.
type Tx[S, X any] struct {
	View[S, X]
	seq uint64
}

// View runs fn with the read lock held. The lock is released however fn
// returns, panics included.
func (t *Table[S, X]) View(fn func(v View[S, X])) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	fn(View[S, X]{t: t})
}

// Update runs fn with the write lock held. The lock is released however fn
// returns, panics included.
func (t *Table[S, X]) Update(fn func(tx Tx[S, X])) {
	t.mu.Lock()
	defer t.end()
	fn(t.begin())
}

// end retires the live Tx and releases the write lock.
func (t *Table[S, X]) end() {
	t.liveSeq = 0
	t.mu.Unlock()
}

// begin issues a Tx for the current write-lock holder.
func (t *Table[S, X]) begin() Tx[S, X] {
	t.seqs++
	t.liveSeq = t.seqs
	return Tx[S, X]{View: View[S, X]{t: t}, seq: t.seqs}
}

// live returns the table when tx still holds the write lock.
func (tx Tx[S, X]) live() *Table[S, X] {
	if tx.t.liveSeq != tx.seq {
		panic("sessiontable: Tx used outside the Update that issued it")
	}
	return tx.t
}

func (v View[S, X]) Get(key string) S                   { return v.t.Get(key) }
func (v View[S, X]) Lookup(key string) (S, bool)        { return v.t.Lookup(key) }
func (v View[S, X]) Len() int                           { return v.t.Len() }
func (v View[S, X]) All() iter.Seq2[string, S]          { return v.t.All() }
func (v View[S, X]) KeysOfChat(chat string) []string    { return v.t.KeysOfChat(chat) }
func (v View[S, X]) ChatHasSessions(chat string) bool   { return v.t.ChatHasSessions(chat) }
func (v View[S, X]) KeyForHash(h string) (string, bool) { return v.t.KeyForHash(h) }
func (v View[S, X]) KeyForID(id string) (string, bool)  { return v.t.KeyForID(id) }
func (v View[S, X]) Active() int64                      { return v.t.Active() }
func (v View[S, X]) Gen() uint64                        { return v.t.Gen() }
func (v View[S, X]) Dirty() bool                        { return v.t.Dirty() }
func (v View[S, X]) Check(idOf func(S) string) []string { return v.t.Check(idOf) }

// Ext is the caller's state kept under the table's lock. Through a View it
// must only be read.
func (v View[S, X]) Ext() *X { return &v.t.ext }

// Ext is the caller's state kept under the table's lock, for writing.
func (tx Tx[S, X]) Ext() *X { return &tx.live().ext }

func (tx Tx[S, X]) Put(key string, s S)             { tx.live().Put(key, s) }
func (tx Tx[S, X]) Delete(key string)               { tx.live().Delete(key) }
func (tx Tx[S, X]) SetID(id, key string)            { tx.live().SetID(id, key) }
func (tx Tx[S, X]) ClearID(id string)               { tx.live().ClearID(id) }
func (tx Tx[S, X]) ClearIDIfOwnedBy(id, key string) { tx.live().ClearIDIfOwnedBy(id, key) }
func (tx Tx[S, X]) AddActive(n int64) int64         { return tx.live().AddActive(n) }
func (tx Tx[S, X]) SetActive(n int64)               { tx.live().SetActive(n) }
func (tx Tx[S, X]) BumpGen()                        { tx.live().BumpGen() }
func (tx Tx[S, X]) MarkChanged()                    { tx.live().MarkChanged() }
func (tx Tx[S, X]) SetDirty(d bool)                 { tx.live().SetDirty(d) }

// Broadcast wakes every Wait.
func (tx Tx[S, X]) Broadcast() { tx.live().cond.Broadcast() }

// Wait releases the lock until Broadcast, then takes it again. Everything
// read before Wait may have changed after it; call it in a loop re-checking
// the condition waited for.
func (tx Tx[S, X]) Wait() {
	t := tx.live()
	t.liveSeq = 0
	t.cond.Wait()
	t.liveSeq = tx.seq
}

// Unlocked runs fn with the lock released and takes it again afterwards, for
// slow work (closing a process, copying history) inside a transaction.
// Everything read before Unlocked may have changed after it, and tx's
// mutators panic inside fn.
func (tx Tx[S, X]) Unlocked(fn func()) {
	t := tx.live()
	t.liveSeq = 0
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.liveSeq = tx.seq
	}()
	fn()
}

// AssumeLocked returns a Tx for code that already holds the write lock
// through Lock. Transitional: it exists while call sites move to Update, and
// goes with Lock / Unlock.
func (t *Table[S, X]) AssumeLocked() Tx[S, X] { return t.begin() }

// Load returns key's session (zero S when none) under the read lock, for a
// caller that needs that one value and nothing else consistent with it.
func (t *Table[S, X]) Load(key string) S {
	t.mu.RLock()
	s := t.sessions[key]
	t.mu.RUnlock()
	return s
}

// Count returns the number of sessions and the live-process count, read
// together under the read lock.
func (t *Table[S, X]) Count() (sessions int, active int64) {
	t.mu.RLock()
	sessions, active = len(t.sessions), t.active.Load()
	t.mu.RUnlock()
	return sessions, active
}

// Healthy reports whether the read lock can be taken right now, for a
// liveness probe that must not block behind a stuck holder.
func (t *Table[S, X]) Healthy() bool {
	if !t.mu.TryRLock() {
		return false
	}
	t.mu.RUnlock()
	return true
}
