package server

import (
	"runtime"
	"testing"
)

// TestTailerSubsPool_ZeroAllocSteadyState pins R245-PERF-15 (#865):
// once a slice has cycled through the pool (acquire → append → release),
// a subsequent acquire-of-the-same-cap must reuse the backing array
// rather than allocate a new one. We assert the pool's contract by
// running a hot loop and showing the per-iteration alloc count is zero.
//
// The first acquire pre-warms the pool so the test does not count the
// initial New() call. Cap is held at the pool's default (4), which is
// what the pollOnce hot path produces under typical 1-2 subscriber
// loads.
func TestTailerSubsPool_ZeroAllocSteadyState(t *testing.T) {
	// Warm the pool so the New func is not amortised into the measurement.
	warm, h := acquireTailerSubsSlice(4)
	releaseTailerSubsSlice(warm, h)

	allocs := testing.AllocsPerRun(100, func() {
		s, h := acquireTailerSubsSlice(4)
		s = append(s, (*wsClient)(nil), (*wsClient)(nil))
		releaseTailerSubsSlice(s, h)
	})
	if allocs != 0 {
		t.Errorf("acquire+append+release allocs/op = %v, want 0 (pool reuse broken)", allocs)
	}
	runtime.KeepAlive(warm)
}

// TestTailerSubsPool_ReleaseClearsPointers pins the GC contract:
// release MUST nil-out every slot so dropped wsClient pointers become
// GC-eligible immediately. Without this, a hot tailer's pool would
// pin one wsClient per parked slot for the lifetime of the pool entry.
func TestTailerSubsPool_ReleaseClearsPointers(t *testing.T) {
	s, h := acquireTailerSubsSlice(4)
	c1 := &wsClient{}
	c2 := &wsClient{}
	s = append(s, c1, c2)
	releaseTailerSubsSlice(s, h)

	// Re-acquire and inspect the underlying array. The pool may or may
	// not return the SAME backing slice depending on internal state,
	// but if it does we want every slot already cleared.
	s2, h2 := acquireTailerSubsSlice(4)
	full := s2[:cap(s2)] // re-extend over the underlying storage
	for i, c := range full {
		if c != nil {
			t.Errorf("after release, slot %d still holds %p (must be nil for GC)", i, c)
		}
	}
	releaseTailerSubsSlice(s2, h2)
}

// TestTailerSubsPool_GrowsForLargeHint covers the cold path: when the
// requested hint exceeds the pooled slice's capacity, acquire must
// return a slice with cap >= hint rather than truncate. Truncating
// would silently drop subscribers in pollOnce.
func TestTailerSubsPool_GrowsForLargeHint(t *testing.T) {
	// Force the pool to hold a small (cap=4) slice.
	small, hs := acquireTailerSubsSlice(4)
	releaseTailerSubsSlice(small, hs)

	big, hb := acquireTailerSubsSlice(64)
	if cap(big) < 64 {
		t.Errorf("acquireTailerSubsSlice(64) returned cap=%d, want >= 64", cap(big))
	}
	releaseTailerSubsSlice(big, hb)
}

// TestTailerSubsPool_NilHandleIsSafe pins the no-subs branch: pollOnce
// can defer release unconditionally, but when there are no subscribers
// it never calls acquire — so the handle is its zero value. Releasing
// with a nil handle.sp must be a no-op, not a panic.
func TestTailerSubsPool_NilHandleIsSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("nil-handle release panicked: %v", r)
		}
	}()
	releaseTailerSubsSlice(nil, tailerSubsHandle{})
}
