package registry

import (
	"reflect"
	"strings"
	"sync"
	"testing"
)

// TestRegister_HappyPath — the load-bearing case: a clean registration
// followed by a Lookup returns the stored value. Without this even the
// most trivial caller would silently break.
func TestRegister_HappyPath(t *testing.T) {
	t.Parallel()
	r := New[int]("nums")
	if err := r.Register("one", 1); err != nil {
		t.Fatalf("Register(one, 1) = %v; want nil", err)
	}
	got, ok := r.Lookup("one")
	if !ok || got != 1 {
		t.Fatalf("Lookup(one) = (%v, %v); want (1, true)", got, ok)
	}
}

// TestRegister_DuplicateRejected pins the no-last-write-wins contract:
// a duplicate Register must return an error and leave the existing
// entry untouched. The R247 review specifically flagged that
// init()-based registration silently overwrites on a re-import; this
// test guarantees the new pattern won't repeat that mistake.
func TestRegister_DuplicateRejected(t *testing.T) {
	t.Parallel()
	r := New[string]("strings")
	if err := r.Register("k", "first"); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := r.Register("k", "second")
	if err == nil {
		t.Fatalf("duplicate Register returned nil; want non-nil error")
	}
	if !strings.Contains(err.Error(), "strings") || !strings.Contains(err.Error(), "k") {
		t.Errorf("error %q should embed registry name and key for diagnosability", err)
	}
	got, _ := r.Lookup("k")
	if got != "first" {
		t.Errorf("duplicate Register clobbered existing entry: got %q want %q", got, "first")
	}
}

// TestRegister_EmptyKeyRejected — defensive check; an empty key is
// almost always a caller bug (forgot to compute the name string).
func TestRegister_EmptyKeyRejected(t *testing.T) {
	t.Parallel()
	r := New[int]("x")
	if err := r.Register("", 0); err == nil {
		t.Fatalf("Register(\"\", _) returned nil; want error")
	}
}

// TestNames_Sorted documents the load-bearing iteration-order contract
// the existing init()-based registries lack: callers can rely on the
// order to write test assertions that don't go flaky after a Go
// release shuffles map iteration.
func TestNames_Sorted(t *testing.T) {
	t.Parallel()
	r := New[int]("ordered")
	for _, k := range []string{"zebra", "alpha", "mike"} {
		if err := r.Register(k, 0); err != nil {
			t.Fatalf("Register(%q): %v", k, err)
		}
	}
	got := r.Names()
	want := []string{"alpha", "mike", "zebra"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v; want %v (sorted)", got, want)
	}
}

// TestConcurrent — a smoke test that hammering Register/Lookup from
// many goroutines does not race. Caught early, this would have spared
// a postmortem where a test-helper concurrently re-registered the same
// plugin while a live request was looking it up.
func TestConcurrent(t *testing.T) {
	t.Parallel()
	r := New[int]("c")
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := string(rune('a' + (i % 26)))
			_ = r.Register(key, i)
			_, _ = r.Lookup(key)
		}(i)
	}
	wg.Wait()
	if r.Len() == 0 {
		t.Fatalf("expected at least one entry after concurrent Register; got 0")
	}
}
