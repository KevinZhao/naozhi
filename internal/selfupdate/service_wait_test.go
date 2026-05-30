package selfupdate

import (
	"context"
	"testing"
	"time"
)

// withStubbedUnitActive swaps systemdUnitActive for the duration of a test
// and restores it afterward.
func withStubbedUnitActive(t *testing.T, fn func() bool) {
	t.Helper()
	orig := systemdUnitActive
	systemdUnitActive = fn
	t.Cleanup(func() { systemdUnitActive = orig })
}

// withFastPoll shrinks the confirm interval so tests don't sleep for seconds.
func withFastPoll(t *testing.T, d time.Duration) {
	t.Helper()
	orig := restartConfirmInterval
	restartConfirmInterval = d
	t.Cleanup(func() { restartConfirmInterval = orig })
}

// TestWaitServiceActive_ReturnsWhenActive: the unit is already active on the
// first poll → no error, no waiting.
func TestWaitServiceActive_ReturnsWhenActive(t *testing.T) {
	withStubbedUnitActive(t, func() bool { return true })
	withFastPoll(t, time.Millisecond)

	if err := waitServiceActive(context.Background(), time.Second); err != nil {
		t.Fatalf("waitServiceActive: %v", err)
	}
}

// TestWaitServiceActive_WaitsThroughActivating simulates a slow Type=notify
// cold start: the unit reports inactive ("activating") for several polls,
// then flips to active. waitServiceActive must succeed, NOT time out — this
// is the exact scenario that made the synchronous restart falsely "fail".
func TestWaitServiceActive_WaitsThroughActivating(t *testing.T) {
	calls := 0
	withStubbedUnitActive(t, func() bool {
		calls++
		return calls >= 4 // inactive for the first 3 polls, then active
	})
	withFastPoll(t, time.Millisecond)

	if err := waitServiceActive(context.Background(), 2*time.Second); err != nil {
		t.Fatalf("waitServiceActive should succeed once the unit becomes active, got: %v", err)
	}
	if calls < 4 {
		t.Errorf("expected to poll until active (>=4 calls), got %d", calls)
	}
}

// TestWaitServiceActive_TimesOut: the unit never becomes active → an error is
// returned. The caller (upgrade.go) treats this as a warning, NOT a rollback
// trigger; this test only pins that a never-active unit is reported.
func TestWaitServiceActive_TimesOut(t *testing.T) {
	withStubbedUnitActive(t, func() bool { return false })
	withFastPoll(t, time.Millisecond)

	err := waitServiceActive(context.Background(), 20*time.Millisecond)
	if err == nil {
		t.Fatal("waitServiceActive should return an error when the unit never becomes active")
	}
}

// TestWaitServiceActive_HonorsCancel: a cancelled context aborts the wait
// promptly instead of polling until the (long) timeout — so `naozhi upgrade`
// stays interruptible during a slow cold start.
func TestWaitServiceActive_HonorsCancel(t *testing.T) {
	withStubbedUnitActive(t, func() bool { return false })
	withFastPoll(t, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	// A 1h timeout would hang for an hour if cancellation were ignored.
	err := waitServiceActive(ctx, time.Hour)
	if err == nil {
		t.Fatal("waitServiceActive should return an error when ctx is cancelled")
	}
}
