package upstream

import (
	"log/slog"
	"os"
	"regexp"
	"sync"
	"testing"
	"time"
)

// TestHandleConnDrainBudget_PackageLevelVar locks the R51-REL-005 budget
// into the shape contract tests expect: a package-level var (not const)
// so shortened values can be injected from tests without wall-clock
// waits, and a positive duration big enough to cover normal drain but
// small enough that systemd TimeoutStopSec does not fire.
func TestHandleConnDrainBudget_PackageLevelVar(t *testing.T) {
	// Read the connector source so a future refactor that converts the
	// var to const (and therefore blocks test injection) is caught.
	src, err := os.ReadFile("connector.go")
	if err != nil {
		t.Fatalf("read connector.go: %v", err)
	}
	// The declaration line must still be a `var`. A `const` form would
	// prevent tests from shortening the budget to millisecond values —
	// they'd either wait the full 15 s (slow CI) or skip this regression
	// altogether.
	varRe := regexp.MustCompile(`var\s+handleConnDrainBudget\s*=\s*`)
	if !varRe.Match(src) {
		t.Error("handleConnDrainBudget is no longer a package-level var. " +
			"R51-REL-005: tests shorten this to milliseconds to exercise the " +
			"stuck-goroutine budget without 15-second wall-clock waits. If " +
			"you need it to be const, add a testing seam (e.g. an unexported " +
			"drainBudget accessor overridable from *_test.go) before making " +
			"the change.")
	}

	// Sanity: default must be positive and within the systemd TimeoutStopSec
	// envelope (production default 30 s; keep a comfortable margin).
	if handleConnDrainBudget <= 0 {
		t.Errorf("handleConnDrainBudget = %v, want > 0", handleConnDrainBudget)
	}
	if handleConnDrainBudget >= 30*time.Second {
		t.Errorf("handleConnDrainBudget = %v is >= 30 s and risks racing "+
			"systemd TimeoutStopSec on shutdown", handleConnDrainBudget)
	}
}

// TestHandleConnDrain_StuckGoroutineDoesNotPin exercises the budget logic
// in isolation: a wg with one stuck Add(1) goroutine must not block the
// drainer beyond the budget window. This is the scenario R51-REL-005
// protects against (a sess.Send blocked on CLI watchdog timeout).
//
// We replicate the exact deferred-drain pattern used in handleConn so
// any future refactor that inlines the pattern differently (e.g. using
// errgroup or custom semantics) can be compared against this reference.
func TestHandleConnDrain_StuckGoroutineDoesNotPin(t *testing.T) {
	// Inject a millisecond budget for this test only. Restored by cleanup.
	orig := handleConnDrainBudget
	handleConnDrainBudget = 50 * time.Millisecond
	t.Cleanup(func() { handleConnDrainBudget = orig })

	// Swallow the expected "drain exceeded budget" Warn so test output is
	// not noisy. We do not assert on log contents — the timing assertion
	// below is authoritative.
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(origLogger) })

	stuck := make(chan struct{}) // never closed by test — simulates the stuck call
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-stuck // intentionally blocks forever; drained on goroutine leak
	}()

	// Replicate the deferred-drain block verbatim. Time the full
	// sequence — budget hit must yield return within 50 ms + scheduling
	// slack (allow 500 ms to keep CI stable).
	start := time.Now()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("wg.Wait returned unexpectedly — stuck channel is never closed " +
			"in this test, so the drainer must have been budget-terminated")
	case <-time.After(handleConnDrainBudget):
		// Good: we escaped the stuck wg on the budget timer, exactly as
		// handleConn does at shutdown.
	}
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Errorf("drain took %v, want < 500 ms (budget=%v, scheduler slack)",
			elapsed, handleConnDrainBudget)
	}
	if elapsed < handleConnDrainBudget {
		t.Errorf("drain took %v, below the configured budget %v — the "+
			"timer fired early, violating the budget contract",
			elapsed, handleConnDrainBudget)
	}

	// Clean up the stuck goroutine so the test process can exit. In
	// production this goroutine leaks to naozhi teardown (SIGTERM → the OS
	// reaps it) — that's the explicit R51-REL-005 tradeoff.
	close(stuck)
}
