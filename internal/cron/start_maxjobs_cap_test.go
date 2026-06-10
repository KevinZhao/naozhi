package cron

import (
	"path/filepath"
	"testing"
)

// TestStart_MaxJobsCapEnforced covers R250-ARCH-26 (#1187): when the on-disk
// cron_jobs.json holds N jobs and the operator restarts naozhi with a lower
// cron.MaxJobs cap (M < N), Start MUST refuse to register the over-cap
// entries. Without this gate every persisted job loaded into memory and
// only the next AddJob would observe the cap — an advisory cap, not an
// enforced one.
func TestStart_MaxJobsCapEnforced(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cron.json")

	// Phase 1: persist 3 jobs at MaxJobs=10.
	s1 := NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: 10}, SchedulerDeps{})
	if err := s1.Start(); err != nil {
		t.Fatalf("phase 1 Start: %v", err)
	}
	for i, p := range []string{"alpha", "beta", "gamma"} {
		if err := s1.AddJob(&Job{
			Schedule: "@hourly",
			Prompt:   p,
			Platform: "p",
			ChatID:   "c",
		}); err != nil {
			t.Fatalf("AddJob %d: %v", i, err)
		}
	}
	s1.Stop()

	// Phase 2: restart with MaxJobs=2. The third persisted entry must be
	// skipped (logged as over-cap) and len(s.jobs) must clamp at 2.
	s2 := NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: 2}, SchedulerDeps{})
	if err := s2.Start(); err != nil {
		t.Fatalf("phase 2 Start: %v", err)
	}
	defer s2.Stop()

	jobs := s2.ListJobs("p", "c")
	if len(jobs) != 2 {
		t.Fatalf("after restart with MaxJobs=2: loaded %d jobs, want 2", len(jobs))
	}
}

// TestStart_MaxJobsCapAllowsExactlyAtCap pins the boundary: when the
// on-disk count equals the cap, every job loads. The cap is "no MORE
// than maxJobs", not "strictly less than". Mirrors addJobAcquiringLock's
// `len(s.jobs) >= s.maxJobs` rejection condition.
func TestStart_MaxJobsCapAllowsExactlyAtCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cron.json")

	s1 := NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: 10}, SchedulerDeps{})
	if err := s1.Start(); err != nil {
		t.Fatalf("phase 1 Start: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := s1.AddJob(&Job{
			Schedule: "@hourly",
			Prompt:   "p",
			Platform: "p",
			ChatID:   "c",
		}); err != nil {
			t.Fatalf("AddJob %d: %v", i, err)
		}
	}
	s1.Stop()

	s2 := NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: 3}, SchedulerDeps{})
	if err := s2.Start(); err != nil {
		t.Fatalf("phase 2 Start: %v", err)
	}
	defer s2.Stop()

	jobs := s2.ListJobs("p", "c")
	if len(jobs) != 3 {
		t.Fatalf("MaxJobs=3 with 3 persisted: loaded %d, want 3", len(jobs))
	}
}
