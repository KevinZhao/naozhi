package session

import (
	"os"
	"regexp"
	"testing"
)

// TestShutdown_SingleShotContract is the R44-REL-HIST-GOROUTINE pin for the
// "Router is not reusable after Shutdown" contract. Shutdown leaks a wrapper
// goroutine around r.historyWg.Wait() when the 5s bounded wait times out on
// hung filesystem I/O — acceptable because the process terminates moments
// later and OS teardown reaps everything.
//
// The failure mode this contract test guards: if someone makes Shutdown
// reusable (e.g. to support hot reloads or test harnesses that spin a
// router up and down), each cycle that hits the I/O timeout would
// accumulate one orphan goroutine, eventually exhausting the goroutine
// quota. Unlike a plain linter rule, this test reads router.go directly
// and asserts the single-shot primitives remain in place:
//
//  1. `shutdownOnce sync.Once` must still be a field on Router.
//  2. `r.shutdownOnce.Do(r.shutdown)` must be the body of Shutdown.
//  3. The "intentionally left running" comment must survive — it is the
//     pointer future readers follow back to this contract.
//
// If any of these is removed, the author is forced to re-evaluate the
// leak/reuse tradeoff instead of silently creating an accumulation bug.
func TestShutdown_SingleShotContract(t *testing.T) {
	src, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}

	// 1) shutdownOnce field must still exist. A rename is fine (the error
	// message suggests re-reading this test), but deletion means the
	// single-shot invariant vanished.
	if !regexp.MustCompile(`shutdownOnce\s+sync\.Once`).Match(src) {
		t.Error("Router.shutdownOnce sync.Once field no longer present. " +
			"If you intentionally made Shutdown reusable, you MUST also " +
			"reclaim the wrapper goroutine around r.historyWg.Wait() in " +
			"shutdown() — otherwise every shutdown cycle that times out " +
			"on hung I/O leaks a goroutine (R44-REL-HIST-GOROUTINE).")
	}

	// 2) The Once.Do gate on Shutdown must still be the body. A future
	// refactor that dispatches to shutdown() directly (e.g. to add a
	// retry-after-error path) breaks idempotency and re-enters the leak.
	if !regexp.MustCompile(`r\.shutdownOnce\.Do\(r\.shutdown\)`).Match(src) {
		t.Error("Router.Shutdown() no longer routes through shutdownOnce.Do. " +
			"Replacing the Once gate with direct dispatch re-opens both the " +
			"goroutine-leak-on-reuse path (R44-REL-HIST-GOROUTINE) and the " +
			"original R49-REL-SHUTDOWN-ONCE race between broadcast timer " +
			"teardown and second-caller re-entry.")
	}

	// 3) The leak-is-intentional comment must stay as a tripwire. Deleting
	// it suggests someone "cleaned up TODOs" without understanding the
	// tradeoff — the comment is the single piece of text linking the
	// goroutine to the contract in Shutdown's godoc.
	if !regexp.MustCompile(`intentionally left running`).Match(src) {
		t.Error("The 'Goroutine intentionally left running on timeout' comment " +
			"around r.historyWg.Wait() was removed. Keep it: it anchors the " +
			"R44-REL-HIST-GOROUTINE contract and tells the next reader why " +
			"this looks like a leak but is not.")
	}
}
