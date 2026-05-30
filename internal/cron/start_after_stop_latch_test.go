package cron

import (
	"errors"
	"testing"
)

// TestStartAfterStop_RefusesRevive pins R249-ARCH-19 (#984): once Stop() has
// latched, Start() must refuse to revive the instance. The Stop() godoc
// documents that triggerWG / gcWG / runDeadlineWatchdog wrapper goroutines are
// intentionally leaked on budget-exceed because the Scheduler is single-shot;
// a Stop-then-Start would re-enter loadJobs + cron.Start + cold-start GC on an
// instance whose stopCtx is already cancelled, accumulating those orphans
// across lifecycles. The started CAS alone does not cover the case where a
// prior Start failed at loadJobs and reset started=false, so the explicit
// stopped gate is required.
func TestStartAfterStop_RefusesRevive(t *testing.T) {
	t.Parallel()
	fake := &fakeSessionRouter{}
	s := NewScheduler(SchedulerConfig{
		Router:  fake,
		MaxJobs: 5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	s.Stop()

	err := s.Start()
	if !errors.Is(err, ErrSchedulerStopped) {
		t.Fatalf("Start after Stop = %v, want ErrSchedulerStopped", err)
	}
}

// TestStartAfterStop_RefusesEvenWhenStartedReset covers the specific gap the
// started CAS could not: a prior Start that failed at loadJobs resets
// started=false, so the CAS would succeed on a subsequent call. After Stop(),
// the stopped latch must still block the revive. We simulate the reset state
// directly because driving a loadJobs failure requires a corrupt store file;
// the field-level assertion is the minimal regression anchor.
func TestStartAfterStop_RefusesEvenWhenStartedReset(t *testing.T) {
	t.Parallel()
	fake := &fakeSessionRouter{}
	s := NewScheduler(SchedulerConfig{
		Router:  fake,
		MaxJobs: 5,
	})
	s.Stop() // latch stopped without ever starting

	// started is false here (never Start()'d), so the started CAS would
	// otherwise let Start() proceed. The stopped gate must win.
	if s.started.Load() {
		t.Fatal("precondition: started must be false")
	}
	if err := s.Start(); !errors.Is(err, ErrSchedulerStopped) {
		t.Fatalf("Start on stopped-but-never-started = %v, want ErrSchedulerStopped", err)
	}
	if s.started.Load() {
		t.Error("Start must not flip started=true after Stop latch")
	}
}
