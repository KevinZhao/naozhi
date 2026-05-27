package cron

import (
	"testing"
)

// TestEnsureStub_NilRouterReturnsFalse pins #491 (R247-GO-10): when the
// scheduler has no Router wired, registerStubByValue silently no-ops;
// EnsureStub must reflect that by returning false instead of cheerfully
// reporting "registered" — otherwise the dashboard sidebar shows a ghost
// stub the router never accepted.
//
// We exercise the AllowNilRouter path (the only way to reach EnsureStub
// with a nil router without tripping the boot-time slog.Error). A fresh
// scheduler with no jobs already returns false for "key not found" — we
// add a job first so the only remaining failure path is router=nil.
func TestEnsureStub_NilRouterReturnsFalse(t *testing.T) {
	s := NewScheduler(SchedulerConfig{
		MaxJobs:        5,
		AllowNilRouter: true,
	})

	// Inject a job directly so EnsureStub finds it under s.mu.RLock and
	// proceeds to the registerStubByValue call. AddJob would also call
	// registerStubFromJob (now also returning bool) but ignoring the
	// result there is fine — we only assert EnsureStub's bool.
	s.mu.Lock()
	s.jobs["jX"] = &Job{
		ID:       "jX",
		WorkDir:  "/tmp",
		Prompt:   "p",
		Schedule: "0 * * * *",
	}
	s.mu.Unlock()

	if got := s.EnsureStub("cron:jX"); got {
		t.Error("EnsureStub returned true with router=nil (regression of #491)")
	}
}

// TestEnsureStub_MissingJobStillReturnsFalse keeps the existing contract
// covered: malformed key / missing job paths still return false. Anchored
// alongside the #491 fix so a future refactor that conflates the two
// failure modes doesn't accidentally mask one of them.
func TestEnsureStub_MissingJobStillReturnsFalse(t *testing.T) {
	s := NewScheduler(SchedulerConfig{
		MaxJobs:        5,
		AllowNilRouter: true,
	})

	if got := s.EnsureStub("cron:nope"); got {
		t.Error("EnsureStub for unknown job returned true; expected false")
	}
	if got := s.EnsureStub("not-a-cron-key"); got {
		t.Error("EnsureStub for non-cron key returned true; expected false")
	}
	if got := s.EnsureStub("cron:"); got {
		t.Error("EnsureStub for empty job ID returned true; expected false")
	}
}
