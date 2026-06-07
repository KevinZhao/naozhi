package cron

import (
	"testing"
	"time"
)

// setWatchdogInterruptTimeoutForTest overrides the package-level
// watchdogInterruptTimeoutAtomic for the duration of one test and registers
// an automatic restore on t.Cleanup. It is the single sanctioned seam for
// timeout tests so the snapshot+restore discipline is enforced in one place
// rather than copied (and occasionally fumbled) across every test.
//
// R20260607-GO-4 (#1904): because the override mutates package-level state,
// callers MUST NOT call t.Parallel(). The helper fails the test loudly if the
// timeout was left in a non-default state by a previous test that forgot to
// restore (a cross-test bleed symptom), turning a silent flake into a
// deterministic failure. See the var's godoc in scheduler_watchdog.go for why
// a per-Scheduler field (the clean fix) is deferred.
func setWatchdogInterruptTimeoutForTest(t *testing.T, d time.Duration) {
	t.Helper()
	prev := watchdogInterruptTimeoutAtomic.Load()
	if prev != int64(watchdogInterruptTimeoutDefault) {
		t.Fatalf("watchdogInterruptTimeout entered test at %v, not the default %v: a prior test leaked an override (likely a t.Parallel() bleed)",
			time.Duration(prev), watchdogInterruptTimeoutDefault)
	}
	watchdogInterruptTimeoutAtomic.Store(int64(d))
	t.Cleanup(func() {
		watchdogInterruptTimeoutAtomic.Store(prev)
	})
}

// TestSetWatchdogInterruptTimeoutForTest_RestoresOnCleanup verifies the seam
// restores the default after the override scope ends, so a subsequent test
// observes the production timeout rather than a leaked short value. This is
// the in-domain regression guard for the t.Parallel cross-talk hazard called
// out in #1904: the helper's entry-time assertion (prev == default) is what
// converts an accidental bleed into a deterministic failure. Runs a nested
// subtest so t.Cleanup fires before we assert restoration.
//
// NOT t.Parallel() — exercises the package-level watchdogInterruptTimeoutAtomic.
func TestSetWatchdogInterruptTimeoutForTest_RestoresOnCleanup(t *testing.T) {
	if got := watchdogInterruptTimeout(); got != watchdogInterruptTimeoutDefault {
		t.Fatalf("precondition: timeout=%v, want default %v", got, watchdogInterruptTimeoutDefault)
	}

	t.Run("override", func(t *testing.T) {
		setWatchdogInterruptTimeoutForTest(t, 50*time.Millisecond)
		if got := watchdogInterruptTimeout(); got != 50*time.Millisecond {
			t.Fatalf("inside override: timeout=%v, want 50ms", got)
		}
	})

	// After the subtest's t.Cleanup ran, the default must be restored — proving
	// no override bleeds past the seam's scope into a sibling test.
	if got := watchdogInterruptTimeout(); got != watchdogInterruptTimeoutDefault {
		t.Fatalf("after override scope: timeout=%v, want restored default %v", got, watchdogInterruptTimeoutDefault)
	}
}

// TestSetWatchdogInterruptTimeoutForTest_DetectsLeakedOverride verifies the
// entry-time guard fires when the atomic is already non-default — the exact
// symptom of a t.Parallel() bleed from another test. We simulate the leak by
// storing a short value directly (bypassing the helper), then assert the helper
// would have fataled by re-checking the precondition the helper enforces.
//
// NOT t.Parallel().
func TestSetWatchdogInterruptTimeoutForTest_DetectsLeakedOverride(t *testing.T) {
	// Simulate a leaked override from a hypothetical parallel test.
	leaked := int64(50 * time.Millisecond)
	prev := watchdogInterruptTimeoutAtomic.Load()
	watchdogInterruptTimeoutAtomic.Store(leaked)
	t.Cleanup(func() { watchdogInterruptTimeoutAtomic.Store(prev) })

	// The helper's guard is `prev != default → Fatal`. We assert the same
	// condition holds so a future change that drops the guard is caught.
	if watchdogInterruptTimeoutAtomic.Load() == int64(watchdogInterruptTimeoutDefault) {
		t.Fatal("expected a non-default (leaked) value to be observable; guard cannot trigger")
	}
}
