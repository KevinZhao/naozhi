package cron

import (
	"testing"
)

// TestCleanupRunningJobIfIdle_SweepsUnexpectedType pins R040034-GO-7
// (#1392): when runningJobs holds a non-*runInflight value (a
// regression / wrong-type Store from a future caller), cleanup must
// still sweep the entry — but it must also surface the invariant
// violation rather than silently swallowing it.
//
// The slog.Error is plumbed into a journalctl scrape; this test only
// asserts the cleanup behaviour (sweep returns true and the entry
// disappears). The slog severity is verified via inspection of the
// produced log handler in production; tests don't pin slog format.
func TestCleanupRunningJobIfIdle_SweepsUnexpectedType(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)

	jobID := mustGenerateID()
	// Inject a wrong-type sentinel value through sync.Map.Store. The
	// only production caller is jobInflight which always stores a
	// *runInflight; a Store of any other shape is the "future
	// regression" the slog.Error guards against.
	s.runningJobs.Store(jobID, "not-a-runInflight")

	if got := s.cleanupRunningJobIfIdle(jobID); !got {
		t.Fatalf("cleanupRunningJobIfIdle returned false on unexpected-type entry; want true (sweep)")
	}
	if _, ok := s.runningJobs.Load(jobID); ok {
		t.Fatalf("entry remained in map after sweep")
	}
}

// TestCleanupRunningJobIfIdle_SweepsNilInflight covers the nil-pointer
// branch of the same defensive type assertion — a *runInflight whose
// underlying value is nil should also be swept (not double-checked
// against the gate).
func TestCleanupRunningJobIfIdle_SweepsNilInflight(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)

	jobID := mustGenerateID()
	var nilInf *runInflight
	s.runningJobs.Store(jobID, nilInf)

	if got := s.cleanupRunningJobIfIdle(jobID); !got {
		t.Fatalf("cleanupRunningJobIfIdle returned false on nil *runInflight entry; want true")
	}
	if _, ok := s.runningJobs.Load(jobID); ok {
		t.Fatalf("entry remained in map after sweep")
	}
}
