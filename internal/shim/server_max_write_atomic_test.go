package shim

import (
	"sync"
	"testing"
)

// TestMaxWriteLineBytes_AtomicAccess pins R237-CR-4 (#701): the package-level
// maxWriteLineBytes knob is mutated by tests and read by handleClient on the
// hot recv path. Before the conversion to atomic.Int64 this was a plain
// `var int` and `go test -race` could not catch a future test that mutates
// the cap concurrently with a live shim handling traffic. The check below
// races a tight Loader/Storer pair under -race; pre-fix it would tripwire
// the unsynchronised data race detector.
func TestMaxWriteLineBytes_AtomicAccess(t *testing.T) {
	orig := setMaxWriteLineBytes(0)
	defer setMaxWriteLineBytes(orig)

	const iters = 1000
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			setMaxWriteLineBytes(int64(i + 1))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_ = maxWriteLineBytesValue()
		}
	}()
	wg.Wait()
}

// TestMaxWriteLineBytes_DefaultFallback confirms the zero-value fallback path
// returns the compile-time default. The atomic.Int64 zero value would
// otherwise mean "no cap" and could regress to the legacy int default of
// 0 (i.e. reject every write).
func TestMaxWriteLineBytes_DefaultFallback(t *testing.T) {
	orig := setMaxWriteLineBytes(0)
	defer setMaxWriteLineBytes(orig)

	if got := maxWriteLineBytesValue(); got != defaultMaxWriteLineBytes {
		t.Fatalf("default fallback: got %d want %d", got, defaultMaxWriteLineBytes)
	}

	prev := setMaxWriteLineBytes(2048)
	if prev != 0 {
		t.Fatalf("Swap returned %d, want 0", prev)
	}
	if got := maxWriteLineBytesValue(); got != 2048 {
		t.Fatalf("after override: got %d want 2048", got)
	}
}
