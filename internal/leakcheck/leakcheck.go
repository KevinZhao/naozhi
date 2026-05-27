// Package leakcheck is a tiny test-only helper for asserting that a
// piece of code did not leak goroutines.
//
// # Why this package exists (R247-ARCH-22, #679)
//
// A code-review across cron / sysession / router / dispatch found 31
// ad-hoc `sync.WaitGroup` sites; tests built around them assert "Close
// waits for the goroutine to exit" but cannot assert "no other
// goroutine was leaked along the way". The full review proposes
// `go.uber.org/goleak` as the long-term answer; pulling in a new
// external dependency for a P3 cleanup is too heavy.
//
// This package is the minimum-viable seed: a `Check(t)` helper that
// snapshots the running-goroutine count at a defer-friendly point and
// fails the enclosing test if the count grew beyond a small grace
// window by the time the test exits.
//
// The implementation is deliberately tolerant:
//
//   - We allow a small drift (default 2) because the runtime sometimes
//     parks short-lived service goroutines (HTTP idle conns, GC sweeper)
//     that were not started by the code under test. Without the drift
//     window the helper would be too noisy to be useful.
//   - We poll for a bounded retry window because a clean shutdown is
//     never instantaneous: a test that calls `relay.Close()` and
//     immediately checks the count would race the worker goroutines on
//     their way to `runtime.goexit`. A 250ms ramp-down is plenty for
//     anything the test suite legitimately holds.
//   - When the helper does fail, it prints the goroutine stack dump so
//     the postmortem doesn't have to re-run the test under -trace.
//
// Coverage policy: this package is opt-in. Tests that already use
// per-WaitGroup contracts (e.g. node/relay_waitgroup_test.go) get an
// additional safety net by adding `defer leakcheck.Check(t)()` at the
// top — leaks that bypass the WG (a goroutine started outside the WG
// scope) now fail the test instead of silently inflating the
// integration-test runtime.
package leakcheck

import (
	"runtime"
	"strings"
	"time"
)

// DefaultGrace is the maximum number of "extra" goroutines tolerated
// at the end of a test without triggering a leak failure. Two extras
// covers the common case where the runtime parks an HTTP idle-conn
// reaper and a goroutine for the test's t.Cleanup helper.
const DefaultGrace = 2

// DefaultSettleWindow is the bounded retry window we wait for goroutines
// to exit before declaring a leak. 250ms is generous enough for any
// shutdown path the codebase legitimately uses, and short enough that
// a leak still fails the test promptly.
const DefaultSettleWindow = 250 * time.Millisecond

// Check snapshots the current goroutine count and returns a closure
// suitable for `defer` at the top of a test:
//
//	func TestX(t *testing.T) {
//	    defer leakcheck.Check(t)()
//	    ... start workers, do work, shut down ...
//	}
//
// The returned closure waits up to DefaultSettleWindow for the count
// to fall back to the captured baseline+DefaultGrace. If the count
// stays elevated past the window, the test fails with a goroutine
// stack dump.
//
// Check uses runtime.NumGoroutine for the count and runtime.Stack for
// the dump on failure. There is no dependency on testing.M / TestMain
// so unit-test files can opt in one test at a time.
func Check(t TB) func() {
	t.Helper()
	return CheckWith(t, DefaultGrace, DefaultSettleWindow)
}

// TB is the subset of testing.TB this package needs. Exposing the
// subset (rather than taking *testing.T directly) lets the package's
// own self-tests inject a fake to verify the failure path without
// flunking the parent test.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
}

// CheckWith is the parameterised form of [Check]. It is exposed so a
// known-noisy test (one that legitimately leaves a long-lived helper
// goroutine running for the duration of the test, e.g. an httptest
// server's accept loop) can pass a larger grace window without
// disabling the check entirely.
func CheckWith(t TB, grace int, settle time.Duration) func() {
	t.Helper()
	baseline := runtime.NumGoroutine()
	return func() {
		t.Helper()
		// Bounded poll: most shutdown paths complete inside a few ms.
		deadline := time.Now().Add(settle)
		var current int
		for time.Now().Before(deadline) {
			current = runtime.NumGoroutine()
			if current <= baseline+grace {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		// One last read after the window so the failure message is
		// honest about the final count, not a stale poll value.
		current = runtime.NumGoroutine()
		if current <= baseline+grace {
			return
		}
		t.Errorf("leakcheck: goroutine count grew from %d to %d (grace %d, settle %s)\n%s",
			baseline, current, grace, settle, dumpStacks())
	}
}

// dumpStacks returns the current process-wide goroutine stack dump,
// trimmed to a reasonable size so a failing test doesn't flood the
// log output. We cap at 64 KiB which is enough to capture ~30 stacks
// at typical depth — far more than the few extra goroutines any leaky
// test will have lying around.
func dumpStacks() string {
	const cap = 64 << 10
	buf := make([]byte, cap)
	n := runtime.Stack(buf, true)
	out := string(buf[:n])
	// Drop the *runtime.Stack* and *leakcheck.dumpStacks* frames at the
	// top — they're never the cause of the leak and they distract the
	// reader.
	if i := strings.Index(out, "\ngoroutine "); i > 0 {
		out = out[i+1:]
	}
	return out
}
