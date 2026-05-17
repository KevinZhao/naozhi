package textutil

import "sync/atomic"

// LoadAtomicString reads an atomic.Pointer[string], returning "" when never
// stored. Retained as a helper since several callers need the "nil → empty
// string" collapse without writing the same 4-line guard inline.
//
// History: cli.loadAtomicString and session.loadStringAtomic were word-for-word
// identical implementations in two different packages with reversed naming.
// Centralised here so a future variant can land in one place. R219-CR-1.
func LoadAtomicString(v *atomic.Pointer[string]) string {
	if p := v.Load(); p != nil {
		return *p
	}
	return ""
}

// StoreAtomicString writes a string via atomic.Pointer[string]. Addresses
// Go's "addressable value" requirement: &id inside a func body references a
// local copy, but passing the string through this helper makes the pointer
// semantics obvious at call sites.
//
// Fast-path short-circuit (R176-PERF-P1): when the currently stored string
// equals s, skip the store entirely. Many callers (SetBackend / SetCLIName /
// SetCLIVersion at reconnect, lastPrompt / lastActivity under AppendBatch's
// tail loop, deathReason idempotent clears) pass the same value they already
// hold; skipping redundant stores avoids per-call *string heap allocation and
// an atomic write on a cache line that readers poll at high rates (Snapshot /
// sidebar refresh).
//
// Safety: the compare-and-store is not atomic as a pair, so a concurrent
// writer may slip a different value between our Load and Store. That is the
// same race that already exists between two direct .Store calls on the same
// pointer, so semantics are unchanged: we only promise last-writer-wins, and
// the fast-path "skip when equal" preserves that (if our s is equal to the
// observed value, writing s would produce the same visible state regardless
// of the intermediate race).
//
// Callers in cli.EventLog also rely on the fast-path being safe under l.mu —
// the load → compare → store sequence cannot be interleaved with another
// store on the same pointer because every writer in that package serialises
// on the same mutex. Outside that contract, see the safety note above.
// R219-CR-1.
func StoreAtomicString(v *atomic.Pointer[string], s string) {
	if cur := v.Load(); cur != nil && *cur == s {
		return
	}
	p := new(string)
	*p = s
	v.Store(p)
}
