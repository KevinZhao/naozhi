package cron

import (
	"testing"
	"time"
)

// setWatchdogInterruptTimeoutForScheduler overrides the per-Scheduler watchdog
// interrupt timeout for the duration of one test and registers an automatic
// restore on t.Cleanup.
//
// R20260607-GO-4 (#1904): the timeout now lives on a per-Scheduler atomic field
// (Scheduler.watchdogInterruptTimeoutNanos) instead of a package-level var, so
// two tests overriding it on *different* *Scheduler no longer clobber each
// other — they MAY call t.Parallel(). The seam stays so the snapshot+restore
// discipline lives in one place; restore matters because tests sometimes share
// a *Scheduler across subtests.
func setWatchdogInterruptTimeoutForScheduler(t *testing.T, s *Scheduler, d time.Duration) {
	t.Helper()
	prev := s.watchdogInterruptTimeoutNanos.Load()
	s.watchdogInterruptTimeoutNanos.Store(int64(d))
	t.Cleanup(func() {
		s.watchdogInterruptTimeoutNanos.Store(prev)
	})
}

// TestSetWatchdogInterruptTimeoutForScheduler_RestoresOnCleanup verifies the
// seam restores the prior value after the override scope ends. Because the
// override is per-instance now, two such overrides on different Schedulers are
// fully isolated — the cross-test bleed hazard #1904 called out is gone.
func TestSetWatchdogInterruptTimeoutForScheduler_RestoresOnCleanup(t *testing.T) {
	t.Parallel()

	s := &Scheduler{}
	s.watchdogInterruptTimeoutNanos.Store(int64(watchdogInterruptTimeoutDefault))
	if got := s.watchdogInterruptTimeout(); got != watchdogInterruptTimeoutDefault {
		t.Fatalf("precondition: timeout=%v, want default %v", got, watchdogInterruptTimeoutDefault)
	}

	t.Run("override", func(t *testing.T) {
		setWatchdogInterruptTimeoutForScheduler(t, s, 50*time.Millisecond)
		if got := s.watchdogInterruptTimeout(); got != 50*time.Millisecond {
			t.Fatalf("inside override: timeout=%v, want 50ms", got)
		}
	})

	// After the subtest's t.Cleanup ran, the prior value must be restored.
	if got := s.watchdogInterruptTimeout(); got != watchdogInterruptTimeoutDefault {
		t.Fatalf("after override scope: timeout=%v, want restored default %v", got, watchdogInterruptTimeoutDefault)
	}
}

// TestWatchdogInterruptTimeout_PerSchedulerIsolation is the in-domain regression
// guard for #1904: two Schedulers with different overrides must NOT bleed into
// each other. This is exactly the t.Parallel cross-talk the package-level var
// permitted; with a per-instance field the two values are independent.
func TestWatchdogInterruptTimeout_PerSchedulerIsolation(t *testing.T) {
	t.Parallel()

	a := &Scheduler{}
	b := &Scheduler{}
	a.watchdogInterruptTimeoutNanos.Store(int64(watchdogInterruptTimeoutDefault))
	b.watchdogInterruptTimeoutNanos.Store(int64(watchdogInterruptTimeoutDefault))

	setWatchdogInterruptTimeoutForScheduler(t, a, 50*time.Millisecond)
	// b is untouched; its override would have leaked through the old global.
	if got := b.watchdogInterruptTimeout(); got != watchdogInterruptTimeoutDefault {
		t.Fatalf("scheduler b timeout=%v, want default %v — per-instance isolation broken (#1904)", got, watchdogInterruptTimeoutDefault)
	}
	if got := a.watchdogInterruptTimeout(); got != 50*time.Millisecond {
		t.Fatalf("scheduler a timeout=%v, want 50ms override", got)
	}
}

// TestNewScheduler_SeedsWatchdogTimeoutDefault pins that the production
// constructor seeds the per-instance timeout to the default (so a Scheduler
// built via NewScheduler bounds InterruptViaControl at 3s, not 0 → instant
// timeout). The zero-value &Scheduler{} path is covered separately by the
// runDeadlineWatchdog timeout<=0 fallback.
func TestNewScheduler_SeedsWatchdogTimeoutDefault(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{AllowNilRouter: true})
	if got := s.watchdogInterruptTimeout(); got != watchdogInterruptTimeoutDefault {
		t.Fatalf("NewScheduler watchdog timeout=%v, want default %v", got, watchdogInterruptTimeoutDefault)
	}
}
