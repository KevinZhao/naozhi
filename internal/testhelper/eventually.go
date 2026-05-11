// Package testhelper provides shared test utilities.
//
// The most common helper is Eventually, which polls a condition until it
// returns true or a timeout elapses. Tests that previously wrote
// `time.Sleep(N); if !cond { t.Fatal(...) }` should migrate to Eventually
// so CI flakiness (slow runners, -race overhead) produces a clear
// diagnostic instead of a hard-coded timing failure.
package testhelper

import (
	"testing"
	"time"
)

// defaultInterval is the poll cadence used by Eventually. 10ms is a
// reasonable balance between responsiveness and scheduler overhead.
const defaultInterval = 10 * time.Millisecond

// Eventually polls cond() until it returns true, or timeout elapses.
//
// Poll interval is 10ms. On timeout it emits t.Fatalf with the timeout
// and caller-supplied msg so flaky tests under CI load give clear
// evidence.
func Eventually(t testing.TB, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	EventuallyWithInterval(t, cond, timeout, defaultInterval, msg)
}

// EventuallyWithInterval is the variant with caller-supplied poll cadence.
// Used when 10ms is too tight (noisy test) or too loose (fast signal).
func EventuallyWithInterval(t testing.TB, cond func() bool, timeout, interval time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(interval)
	}
	// Final attempt at the deadline boundary so tests on slow CI get one
	// last evaluation after the timeout sleep.
	if cond() {
		return
	}
	t.Fatalf("Eventually timed out after %v: %s", timeout, msg)
}
