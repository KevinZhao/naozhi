// Package testhelper provides shared test utilities, chiefly Eventually, which
// polls a condition instead of a fixed sleep so slow CI gives a clear diagnostic.
package testhelper

import (
	"testing"
	"time"
)

// defaultInterval is the poll cadence used by Eventually.
const defaultInterval = 10 * time.Millisecond

// Eventually polls cond() every 10ms until it returns true or timeout elapses,
// then fails the test with msg.
func Eventually(t testing.TB, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	EventuallyWithInterval(t, cond, timeout, defaultInterval, msg)
}

// EventuallyWithInterval is Eventually with a caller-supplied poll cadence.
func EventuallyWithInterval(t testing.TB, cond func() bool, timeout, interval time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(interval)
	}
	// Final attempt at the deadline boundary.
	if cond() {
		return
	}
	t.Fatalf("Eventually timed out after %v: %s", timeout, msg)
}
