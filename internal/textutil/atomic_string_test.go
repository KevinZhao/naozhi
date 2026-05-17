package textutil

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestLoadAtomicString_Empty(t *testing.T) {
	t.Parallel()
	var v atomic.Pointer[string]
	if got := LoadAtomicString(&v); got != "" {
		t.Errorf("LoadAtomicString(nil ptr) = %q, want \"\"", got)
	}
}

func TestStoreAtomicString_RoundTrip(t *testing.T) {
	t.Parallel()
	var v atomic.Pointer[string]
	StoreAtomicString(&v, "Bash")
	if got := LoadAtomicString(&v); got != "Bash" {
		t.Errorf("after Store(\"Bash\"), Load = %q, want \"Bash\"", got)
	}
	StoreAtomicString(&v, "Read")
	if got := LoadAtomicString(&v); got != "Read" {
		t.Errorf("after Store(\"Read\"), Load = %q, want \"Read\"", got)
	}
}

// TestStoreAtomicString_FastPath checks the equal-value short-circuit: a Store
// with the same value as the current pointee MUST NOT swap to a freshly
// allocated *string. Verified by capturing the pointee address and asserting
// it stays stable across redundant Stores.
func TestStoreAtomicString_FastPath(t *testing.T) {
	t.Parallel()
	var v atomic.Pointer[string]
	StoreAtomicString(&v, "Bash")
	first := v.Load()
	StoreAtomicString(&v, "Bash")
	StoreAtomicString(&v, "Bash")
	if got := v.Load(); got != first {
		t.Errorf("redundant Store reallocated *string: first=%p got=%p", first, got)
	}
}

// TestStoreAtomicString_EmptyValue covers the explicit-empty-string store case
// (distinct from the zero-pointer state checked above): callers like
// clearDeathReason rely on Store("") materialising a non-nil *string so the
// load path returns "" without going through the nil-pointer branch.
func TestStoreAtomicString_EmptyValue(t *testing.T) {
	t.Parallel()
	var v atomic.Pointer[string]
	StoreAtomicString(&v, "")
	if p := v.Load(); p == nil {
		t.Fatalf("Store(\"\") left pointer nil")
	}
	if got := LoadAtomicString(&v); got != "" {
		t.Errorf("Store(\"\") then Load = %q, want \"\"", got)
	}
}

// TestStoreAtomicString_ConcurrentWriters exercises the documented
// last-writer-wins contract under real contention so any future tightening of
// the fast-path keeps the published guarantee.
func TestStoreAtomicString_ConcurrentWriters(t *testing.T) {
	t.Parallel()
	var v atomic.Pointer[string]
	const writers = 32
	const itersPerWriter = 200
	values := []string{"a", "b", "c", "d"}
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		i := i
		go func() {
			defer wg.Done()
			for j := 0; j < itersPerWriter; j++ {
				StoreAtomicString(&v, values[(i+j)%len(values)])
			}
		}()
	}
	wg.Wait()
	got := LoadAtomicString(&v)
	found := false
	for _, want := range values {
		if got == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("post-contention Load = %q, want one of %v", got, values)
	}
}
