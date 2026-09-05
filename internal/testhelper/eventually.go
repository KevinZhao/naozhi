// Package testhelper provides shared test utilities, chiefly Eventually, which
// polls a condition instead of a fixed sleep so slow CI gives a clear diagnostic.
//
// Replacing a bare time.Sleep (the ratchet in sleep_ratchet_test.go rejects
// new ones):
//
//	// Waiting for an async counter / state flip:
//	//   time.Sleep(30 * time.Millisecond)
//	//   if got := counter.Load(); got != 2 { ... }
//	testhelper.Eventually(t, func() bool { return counter.Load() == 2 },
//		2*time.Second, "OnRegister should fire twice")
//
//	// Waiting for a callback: prefer a channel join over polling.
//	done := make(chan struct{}, 1)
//	obj.OnEvent = func() { done <- struct{}{} }
//	select {
//	case <-done:
//	case <-time.After(2 * time.Second):
//		t.Fatal("OnEvent never fired")
//	}
//
//	// Waiting for a file / external resource with a slow producer:
//	testhelper.EventuallyWithInterval(t, func() bool {
//		_, err := os.Stat(path)
//		return err == nil
//	}, 5*time.Second, 100*time.Millisecond, "sidecar file should appear")
//
// A sleep that is genuinely about elapsed time (creating a measurable
// duration, expiring a TTL) keeps time.Sleep with a `// sleep-ok: <reason>`
// annotation on the same line.
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
