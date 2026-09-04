package textutil

import "sync/atomic"

// LoadAtomicString reads an atomic.Pointer[string], returning "" when never
// stored.
func LoadAtomicString(v *atomic.Pointer[string]) string {
	if p := v.Load(); p != nil {
		return *p
	}
	return ""
}

// StoreAtomicString writes a string via atomic.Pointer[string]. When the
// currently stored value already equals s the store is skipped, avoiding a
// per-call *string allocation and an atomic write on a cache line that
// readers poll at high rates.
//
// The load → compare → store pair is not atomic, but a concurrent writer
// slipping in between is the same last-writer-wins race two direct .Store
// calls already have; skipping an equal value cannot change the visible
// outcome. cli.EventLog additionally serialises all writers under l.mu.
func StoreAtomicString(v *atomic.Pointer[string], s string) {
	if cur := v.Load(); cur != nil && *cur == s {
		return
	}
	p := new(string)
	*p = s
	v.Store(p)
}
