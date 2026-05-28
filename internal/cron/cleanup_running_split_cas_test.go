package cron

import (
	"testing"
)

// TestCleanupRunningJobIfIdle_ClaimsGateBeforeDelete pins R040034-GO-3
// (#1389): cleanupRunningJobIfIdle must claim the running CAS gate
// before removing the map entry. Without the claim, a concurrent
// executeOpt could CAS-win the gate between cleanup's running.Load()
// and CompareAndDelete, leaving the active *runInflight orphaned (off
// the map, but still running) while a fresh jobInflight call returns a
// distinct *runInflight pointer for any new executeOpt — split CAS
// gate, double execution.
//
// Test pre-claims the gate to simulate "in-flight execute()": cleanup
// must return false and leave the entry intact.
func TestCleanupRunningJobIfIdle_ClaimsGateBeforeDelete(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{MaxJobs: 5})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)

	jobID := mustGenerateID()
	inf := s.jobInflight(jobID)
	// Pre-claim the gate as if executeOpt is in flight.
	if !inf.running.CompareAndSwap(false, true) {
		t.Fatalf("pre-claim CAS failed; expected fresh inflight to start at false")
	}
	t.Cleanup(func() { inf.running.Store(false) })

	// cleanup must observe running==true (CAS fails) and bail out
	// without deleting the entry.
	if got := s.cleanupRunningJobIfIdle(jobID); got != false {
		t.Fatalf("cleanupRunningJobIfIdle returned true while gate held; want false (leak THIS entry)")
	}
	if _, ok := s.runningJobs.Load(jobID); !ok {
		t.Fatalf("entry was removed despite gate held — split CAS gate hazard")
	}
}

// TestCleanupRunningJobIfIdle_DeletesIdleEntry pins the happy path: when
// the gate is idle, cleanup claims it, deletes the map entry, and
// releases the gate. A subsequent jobInflight call returns a FRESH
// pointer (the old inf is no longer reachable through the map).
func TestCleanupRunningJobIfIdle_DeletesIdleEntry(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{MaxJobs: 5})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)

	jobID := mustGenerateID()
	inf := s.jobInflight(jobID)
	// Confirm gate is idle pre-cleanup.
	if inf.running.Load() {
		t.Fatalf("fresh inflight unexpectedly running")
	}

	if got := s.cleanupRunningJobIfIdle(jobID); !got {
		t.Fatalf("cleanupRunningJobIfIdle returned false on idle entry")
	}
	if _, ok := s.runningJobs.Load(jobID); ok {
		t.Fatalf("entry remained in map after successful cleanup")
	}

	// Fresh jobInflight call after cleanup must return a NEW pointer.
	// The old inf could still be referenced by the test, but the map
	// entry is gone so any new caller starts from a fresh CAS gate.
	fresh := s.jobInflight(jobID)
	if fresh == inf {
		t.Fatalf("jobInflight after cleanup returned the same *runInflight; expected fresh allocation")
	}
}
