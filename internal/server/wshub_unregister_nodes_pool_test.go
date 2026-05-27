package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/node"
)

// TestUnregisterNodesPool_Reuses asserts the per-disconnect snapshot slice
// pool used by Hub.unregister actually reuses the underlying backing array.
// Without the pool every multi-node disconnect allocated a fresh
// `make([]node.Conn, 0, len(h.nodes))` (R249-PERF-6 / #927). The pool turns
// that into one allocation amortised across the steady-state reconnect
// stream of mobile clients.
//
// Direct pool exercise rather than a full Hub.unregister round-trip — the
// pool's Get→fill→Put semantics are the load-bearing invariant; spinning
// up an entire Hub + node.Conn stack just to observe a single allocation
// would be far heavier than the property under test.
func TestUnregisterNodesPool_Reuses(t *testing.T) {
	first := unregisterNodesPool.Get().(*[]node.Conn)
	if first == nil {
		t.Fatal("pool returned nil pointer")
	}
	if cap(*first) < 4 {
		t.Errorf("New() should seed cap>=4 to skip the first realloc; got cap=%d", cap(*first))
	}

	// Simulate the unregister fill + clear cycle.
	*first = append(*first, nil, nil, nil)
	for i := range *first {
		(*first)[i] = nil
	}
	*first = (*first)[:0]
	unregisterNodesPool.Put(first)

	// A second Get should at least observe a non-nil pointer with a usable
	// backing array; sync.Pool semantics do not strictly guarantee the
	// exact same pointer comes back (GC may have drained the local pool),
	// but Cap should be at least 4 because that's the New() seed.
	second := unregisterNodesPool.Get().(*[]node.Conn)
	if second == nil {
		t.Fatal("pool returned nil pointer on second Get")
	}
	if cap(*second) < 4 {
		t.Errorf("second Get cap < seed: got %d", cap(*second))
	}
	if len(*second) != 0 {
		t.Errorf("pool returned dirty slice (len=%d); Hub.unregister relies on len==0", len(*second))
	}
	unregisterNodesPool.Put(second)
}
