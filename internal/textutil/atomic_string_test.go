package textutil

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestStoreAtomicString_RoundTrip covers the zero-pointer Load, value
// round-trips, the equal-value fast path (no fresh *string allocation), and
// Store("") materialising a non-nil pointer (clearDeathReason relies on it).
func TestStoreAtomicString_RoundTrip(t *testing.T) {
	t.Parallel()
	var v atomic.Pointer[string]
	if got := LoadAtomicString(&v); got != "" {
		t.Errorf("LoadAtomicString(nil ptr) = %q, want \"\"", got)
	}
	StoreAtomicString(&v, "Bash")
	first := v.Load()
	if first == nil || *first != "Bash" {
		t.Fatalf("after Store(\"Bash\"), Load = %v, want \"Bash\"", first)
	}
	StoreAtomicString(&v, "Bash")
	if got := v.Load(); got != first {
		t.Errorf("redundant Store reallocated *string: first=%p got=%p", first, got)
	}
	StoreAtomicString(&v, "Read")
	if got := LoadAtomicString(&v); got != "Read" {
		t.Errorf("after Store(\"Read\"), Load = %q, want \"Read\"", got)
	}
	StoreAtomicString(&v, "")
	if v.Load() == nil {
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
