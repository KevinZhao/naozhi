// Package leakcheck is a tiny test-only helper for asserting that a piece of
// code did not leak goroutines: Check snapshots runtime.NumGoroutine and, when
// the deferred closure runs, fails the test if the count stayed above
// baseline+grace after a bounded settle window, printing a stack dump (#679).
//
// It is deliberately tolerant: a small grace absorbs runtime service
// goroutines (HTTP idle conns, GC sweeper) not started by the code under test,
// and the settle window absorbs workers still on their way to runtime.goexit.
// Opt-in per test via `defer leakcheck.Check(t)()`.
package leakcheck

import (
	"runtime"
	"strings"
	"time"
)

// DefaultGrace is the number of extra goroutines tolerated at test end; two
// covers an HTTP idle-conn reaper plus a t.Cleanup helper.
const DefaultGrace = 2

// DefaultSettleWindow bounds how long the check waits for goroutines to exit
// before declaring a leak.
const DefaultSettleWindow = 250 * time.Millisecond

// Check snapshots the current goroutine count and returns a closure for
// `defer` at the top of a test:
//
//	defer leakcheck.Check(t)()
//
// The closure applies DefaultGrace / DefaultSettleWindow via CheckWith.
func Check(t TB) func() {
	t.Helper()
	return CheckWith(t, DefaultGrace, DefaultSettleWindow)
}

// TB is the subset of testing.TB this package needs; self-tests inject a fake
// to verify the failure path.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
}

// CheckWith is the parameterised form of [Check] for tests that legitimately
// keep a long-lived helper goroutine (e.g. an httptest accept loop).
func CheckWith(t TB, grace int, settle time.Duration) func() {
	t.Helper()
	baseline := runtime.NumGoroutine()
	return func() {
		t.Helper()
		deadline := time.Now().Add(settle)
		var current int
		for time.Now().Before(deadline) {
			current = runtime.NumGoroutine()
			if current <= baseline+grace {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		// Final read so the failure message reports the current count.
		current = runtime.NumGoroutine()
		if current <= baseline+grace {
			return
		}
		t.Errorf("leakcheck: goroutine count grew from %d to %d (grace %d, settle %s)\n%s",
			baseline, current, grace, settle, dumpStacks())
	}
}

// dumpStacks returns the goroutine stack dump capped at 64 KiB (~30 stacks)
// so a failing test does not flood the log.
func dumpStacks() string {
	const cap = 64 << 10
	buf := make([]byte, cap)
	n := runtime.Stack(buf, true)
	out := string(buf[:n])
	// Drop the runtime.Stack / dumpStacks frames at the top.
	if i := strings.Index(out, "\ngoroutine "); i > 0 {
		out = out[i+1:]
	}
	return out
}
