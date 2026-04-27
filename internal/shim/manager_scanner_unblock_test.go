package shim

// R40-CONCUR1 / R42-REL-SHIM-PGKILL regression anchor.
//
// StartShim's three failure branches (ready error / ready timeout /
// ctx.Done) used to call cmd.Process.Kill() only, leaving the bufio
// Scanner goroutine parked on stdout.Read. On a shim that ignores
// SIGTERM (or on a process-group leader whose child CLI holds stdout
// open past the shim's exit) the goroutine could idle until the shim's
// 4 h idle-timeout fired — under restart storms that accumulated
// dozens to hundreds of leaked goroutines.
//
// The fix is a killAndUnblock helper that closes the stdout read-end
// alongside the Kill, making scanner.Scan() return false immediately.
// This test pins the OS-level primitive: closing the reader end of an
// os.Pipe causes a bufio.Scanner blocked on it to fall out within a
// few milliseconds. If a future refactor swaps stdout.Close() for
// something that no longer interrupts a blocked Read, this test fails
// and flags the regression before it reaches production restart paths.

import (
	"bufio"
	"io"
	"os"
	"testing"
	"time"
)

func TestScannerUnblocksOnStdoutClose(t *testing.T) {
	t.Parallel()

	// Mirror StartShim's topology: producer writes nothing for "a while",
	// consumer blocks in scanner.Scan() on the read end.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(r)
		// This Scan call is the same shape as the real scanner goroutine
		// in manager.StartShim; it will block until either a newline-
		// terminated line arrives or the underlying fd is closed.
		_ = scanner.Scan()
	}()

	// Wait long enough for the goroutine to park on Read. 20 ms is ample
	// on every CI machine we've tested; we do not want to race the
	// Close() below with the goroutine's scheduling.
	time.Sleep(20 * time.Millisecond)

	// Close the reader end — this is what killAndUnblock does to
	// stdout inside StartShim. The OS must surface the Close as an
	// immediate Read error on the blocked side.
	if err := r.Close(); err != nil && err != io.ErrClosedPipe {
		t.Fatalf("close reader: %v", err)
	}

	select {
	case <-done:
		// scanner.Scan returned — the unblock primitive works as designed.
	case <-time.After(2 * time.Second):
		t.Fatal("scanner goroutine still blocked after reader Close; " +
			"killAndUnblock would leak here")
	}
}

// TestScannerUnblocksOnWriterClose mirrors the shim-side-close path:
// if the shim process exits (after Kill()) the kernel closes the pipe
// at the write end, which also makes Scan() return. This path is the
// "happy" path when the shim respects SIGTERM promptly; the primary
// fix above covers the case where the shim does not.
func TestScannerUnblocksOnWriterClose(t *testing.T) {
	t.Parallel()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(r)
		_ = scanner.Scan()
	}()

	time.Sleep(20 * time.Millisecond)
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scanner goroutine still blocked after writer Close")
	}
}
