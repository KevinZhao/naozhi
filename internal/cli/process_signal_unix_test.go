//go:build !windows

package cli

import (
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// TestProcess_Kill_SendsSIGUSR2ToShim locks in the behaviour added for
// UCCLEP-2026-04-26: when the Process carries a non-zero shimPID, Kill
// must send SIGUSR2 so the shim's immediate-shutdown path runs and the
// socket file is unlinked. Without this, the Kill fallback from Close's
// timeout still leaves the socket listening for up to 30s and the next
// StartShim for the same key fails the dial-first guard.
//
// Uses the test process's own PID as the shim PID so the handler observes
// the signal without spawning a child; the handler is removed at cleanup
// to avoid leaking SIGUSR2 handling into other tests in the same binary.
//
// NOT t.Parallel() — registers a process-global SIGUSR2 handler via
// signal.Notify. Concurrent tests that also register/observe SIGUSR2 would
// cross-trigger (one test's Kill → other test's sigCh). Serial only.
func TestProcess_Kill_SendsSIGUSR2ToShim(t *testing.T) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR2)
	t.Cleanup(func() { signal.Stop(sigCh) })

	p, _ := shimTestPair(&ClaudeProtocol{})
	p.shimPID = os.Getpid()

	p.Kill()

	select {
	case <-sigCh:
		// good: Kill sent SIGUSR2 to shim
	case <-time.After(2 * time.Second):
		t.Fatal("Kill() did not send SIGUSR2 within 2s")
	}
}

// TestProcess_Kill_NoSIGUSR2WhenShimPIDZero ensures the fallback signal
// path is strictly gated on shimPID > 0 — tests and legacy callers that
// construct Process without hello data must not trigger a misdirected
// signal to PID 0 (which is "send to every process in the caller's
// process group" on Unix, a dangerous broadcast).
//
// NOT t.Parallel() — same process-global signal registration rationale
// as TestProcess_Kill_SendsSIGUSR2ToShim above.
func TestProcess_Kill_NoSIGUSR2WhenShimPIDZero(t *testing.T) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR2)
	t.Cleanup(func() { signal.Stop(sigCh) })

	p, _ := shimTestPair(&ClaudeProtocol{})
	if p.shimPID != 0 {
		t.Fatalf("precondition: shimPID = %d, want 0", p.shimPID)
	}

	p.Kill()

	select {
	case <-sigCh:
		t.Fatal("Kill() sent SIGUSR2 despite shimPID == 0")
	case <-time.After(200 * time.Millisecond):
		// good: no signal
	}
}
