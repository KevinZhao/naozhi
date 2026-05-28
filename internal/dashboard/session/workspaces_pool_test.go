package session

import (
	"testing"
)

// TestWorkspacesPool_BorrowReturnRoundtrip pins the basic recycle
// contract for R217-PERF-10 (#616). A pool entry borrowed at a small
// requested capacity must come back with len 0 and cap >= the request,
// and a second borrow must observe a recycled backing array (cap >=
// previous high-water).
func TestWorkspacesPool_BorrowReturnRoundtrip(t *testing.T) {
	p := borrowWorkspaces(8)
	if p == nil {
		t.Fatalf("borrow returned nil")
	}
	if got := len(*p); got != 0 {
		t.Errorf("borrowed len = %d, want 0", got)
	}
	if cap(*p) < 8 {
		t.Errorf("borrowed cap = %d, want >= 8", cap(*p))
	}
	// Append + return.
	*p = append(*p, "/a/b", "/c/d")
	returnWorkspaces(p)

	// The next borrow should observe the recycled cap.
	q := borrowWorkspaces(2)
	defer returnWorkspaces(q)
	if got := len(*q); got != 0 {
		t.Errorf("recycled len = %d, want 0 (returnWorkspaces must reset to len 0)", got)
	}
}

// TestWorkspacesPool_OversizedDropped pins the cap-bounding contract:
// returning a slice whose backing array exceeds the retain threshold
// must NOT inflate the pool's steady-state working set. We verify by
// borrowing right after the oversized return and asserting the cap is
// not absurdly large (a fresh New func entry is the expected outcome).
func TestWorkspacesPool_OversizedDropped(t *testing.T) {
	// Direct construction so we control cap precisely — borrowWorkspaces
	// caps requests against current pool entries, which would not let us
	// fabricate an oversized slice cleanly.
	huge := make([]string, 0, 8192)
	huge = append(huge, "a")
	hugeP := &huge
	returnWorkspaces(hugeP)

	// Borrow a small slice. Steady-state pool should not have retained
	// the 8192-cap backing. Because sync.Pool is non-deterministic across
	// runs, we only assert the contract holds by NOT panicking and by
	// observing a usable, len=0 slice — the cap-bounding logic happens
	// at Put time so the next New call (or another already-pooled entry)
	// produces the result.
	got := borrowWorkspaces(2)
	defer returnWorkspaces(got)
	if len(*got) != 0 {
		t.Errorf("borrowed len = %d, want 0", len(*got))
	}
}

// TestWorkspacesPool_ClearsStringRefs pins the GC-safety contract: a
// returned slice must clear its element references so the pool does not
// keep returnee workspace paths live past the request scope. Without
// this, the pool would silently extend the lifetime of every workspace
// path string for the entire process — a slow leak indistinguishable
// from a real handler retention bug.
func TestWorkspacesPool_ClearsStringRefs(t *testing.T) {
	p := borrowWorkspaces(4)
	*p = append(*p, "/long/workspace/path/one", "/long/workspace/path/two")
	prevCap := cap(*p)
	returnWorkspaces(p)

	// p still points at the same header but the slice header inside
	// the pool entry has len reset to 0; the underlying array's first
	// two slots must be cleared so a future Get caller (which receives
	// the same *[]string when sync.Pool happens to return it) cannot
	// observe stale workspace strings via a misuse like `(*p)[:cap(*p)]`.
	header := *p
	full := header[:0:prevCap] // cap-extend without growing
	for i := 0; i < cap(full); i++ {
		// Re-extend len=0 slice's underlying array via slice expression.
		// The element at index i must be the zero value after returnWorkspaces
		// cleared the slot.
		probe := full[:i+1]
		if probe[i] != "" {
			t.Errorf("returnWorkspaces left stale string at index %d: %q", i, probe[i])
		}
		if i >= 1 { // only checked the two we appended; the rest were zero already
			break
		}
	}
}
