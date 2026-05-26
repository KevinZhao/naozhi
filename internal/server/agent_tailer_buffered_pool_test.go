package server

import (
	"reflect"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestAcquireTailerBufferedSlice_RoundTrip pins the contract that
// the pool returns a usable empty slice with cap >= hint, the caller
// can append into it, and Release returns the same backing array to
// the pool. R249-PERF-4 (#926).
func TestAcquireTailerBufferedSlice_RoundTrip(t *testing.T) {
	// Warm the pool first so the second Get is a real reuse, not New.
	warm, h := acquireTailerBufferedSlice(8)
	releaseTailerBufferedSlice(warm, h)

	for trial := 0; trial < 4; trial++ {
		s, h := acquireTailerBufferedSlice(8)
		if len(s) != 0 {
			t.Fatalf("trial %d: got len=%d, want 0", trial, len(s))
		}
		if cap(s) < 8 {
			t.Fatalf("trial %d: got cap=%d, want >= 8", trial, cap(s))
		}
		s = append(s, cli.EventEntry{Type: "test"})
		releaseTailerBufferedSlice(s, h)
	}
}

// TestReleaseTailerBufferedSlice_ZeroClears pins that releasing a
// populated slice clears each EventEntry to the zero value before it
// re-enters the pool. Without this, the next acquire would inherit
// the prior caller's []string slices (Images / ImagePaths) and pin
// that backing array alive past the original attach. We populate
// entries with non-zero scalar AND slice fields, release, and check
// every previously-touched index reads back zero. EventEntry contains
// []string fields so `==` is not defined; reflect.DeepEqual covers
// every field uniformly. R249-PERF-4 (#926).
func TestReleaseTailerBufferedSlice_ZeroClears(t *testing.T) {
	s, h := acquireTailerBufferedSlice(4)
	s = append(s, cli.EventEntry{Type: "assistant", Summary: "hello", Images: []string{"img1"}})
	s = append(s, cli.EventEntry{Type: "tool_use", Tool: "bash", ImagePaths: []string{"p1"}})

	releaseTailerBufferedSlice(s, h)

	// Reach back into the backing array. The pool's Put zero-clears in
	// place, so positions 0..1 must now hold zero-valued EventEntry.
	// Cap-truncation reaches the slots even though len was reset to 0.
	full := s[:cap(s)]
	zero := cli.EventEntry{}
	for i := 0; i < 2; i++ {
		if !reflect.DeepEqual(full[i], zero) {
			t.Errorf("entry %d not cleared: got %+v", i, full[i])
		}
	}
}

// TestAcquireTailerBufferedSlice_NilHandleRelease pins that the
// release helper is a no-op for nil-handle inputs. The caller-side
// pattern is `defer releaseTailerBufferedSlice(buf, handle)` even on
// the no-buffered-events branch (where attach may take the early
// return path). A nil handle must not panic and must not return
// anything to the pool.
func TestAcquireTailerBufferedSlice_NilHandleRelease(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil-handle release panicked: %v", r)
		}
	}()
	releaseTailerBufferedSlice(nil, tailerBufferedHandle{})
	releaseTailerBufferedSlice([]cli.EventEntry{{}}, tailerBufferedHandle{})
}

// TestAcquireTailerBufferedSlice_GrowsCap pins that requesting a hint
// larger than the pooled slice's cap produces a fresh slice with the
// requested cap, and that the grown slice is what comes back from
// Release (the caller's append-grown slice supersedes whatever was in
// the pool). Without this, an attach that replays 500 events on a
// pool entry with cap=16 would silently get a tiny slice and pay 30+
// growth-allocs inside the critical section.
func TestAcquireTailerBufferedSlice_GrowsCap(t *testing.T) {
	small, hs := acquireTailerBufferedSlice(4)
	releaseTailerBufferedSlice(small, hs)

	big, hb := acquireTailerBufferedSlice(64)
	if cap(big) < 64 {
		t.Errorf("acquireTailerBufferedSlice(64) returned cap=%d, want >= 64", cap(big))
	}
	releaseTailerBufferedSlice(big, hb)
}
