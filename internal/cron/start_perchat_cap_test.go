package cron

import (
	"path/filepath"
	"testing"
)

// TestStart_PerChatCapEnforced covers R20260613-CR-10 (#2060): the startup
// load path (loadJobs) must enforce maxJobsPerChat just like AddJob does.
// Previously loadJobs went straight to addToChatIndexLocked, so a legacy /
// hand-edited cron_jobs.json whose single chat held more than the cap would
// load every entry — leaving the in-memory chatJobCount above the cap, after
// which AddJob would report "per-chat limit reached" while the operator
// believed there was headroom.
//
// We persist N jobs in one chat at a high per-chat cap, then restart with a
// lower cap (M < N) and assert the loaded count clamps at M.
func TestStart_PerChatCapEnforced(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cron.json")

	// Phase 1: persist 3 jobs in chat (p, c) at a generous per-chat cap.
	s1 := NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: 100, MaxJobsPerChat: 10}, SchedulerDeps{})
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

	// Phase 2: restart with MaxJobsPerChat=2. The third persisted entry must
	// be skipped (logged as over-per-chat) and the loaded count clamps at 2.
	s2 := NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: 100, MaxJobsPerChat: 2}, SchedulerDeps{})
	if err := s2.Start(); err != nil {
		t.Fatalf("phase 2 Start: %v", err)
	}
	defer s2.Stop()

	jobs := s2.ListJobs("p", "c")
	if len(jobs) != 2 {
		t.Fatalf("after restart with MaxJobsPerChat=2: loaded %d jobs, want 2", len(jobs))
	}
	if n := s2.PerChatJobCount("p", "c"); n != 2 {
		t.Fatalf("chatJobCount after clamp = %d, want 2", n)
	}

	// The clamp must keep the in-memory count consistent with the cap so a
	// subsequent AddJob in a DIFFERENT chat still works, while the capped chat
	// is genuinely full (not falsely over-full from an unclamped load).
	if err := s2.AddJob(&Job{Schedule: "@hourly", Prompt: "x", Platform: "p", ChatID: "c"}); err == nil {
		t.Fatalf("AddJob into a full chat should hit the per-chat cap, got nil error")
	}
	if err := s2.AddJob(&Job{Schedule: "@hourly", Prompt: "y", Platform: "p", ChatID: "other"}); err != nil {
		t.Fatalf("AddJob into a fresh chat should succeed: %v", err)
	}
}

// TestStart_PerChatCapAllowsExactlyAtCap pins the boundary: when the on-disk
// per-chat count equals the cap, every job in that chat loads. The cap is
// "no MORE than maxJobsPerChat", mirroring addJobAcquiringLock's
// `>= s.maxJobsPerChat` rejection.
func TestStart_PerChatCapAllowsExactlyAtCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cron.json")

	s1 := NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: 100, MaxJobsPerChat: 10}, SchedulerDeps{})
	if err := s1.Start(); err != nil {
		t.Fatalf("phase 1 Start: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := s1.AddJob(&Job{Schedule: "@hourly", Prompt: "p", Platform: "p", ChatID: "c"}); err != nil {
			t.Fatalf("AddJob %d: %v", i, err)
		}
	}
	s1.Stop()

	s2 := NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: 100, MaxJobsPerChat: 3}, SchedulerDeps{})
	if err := s2.Start(); err != nil {
		t.Fatalf("phase 2 Start: %v", err)
	}
	defer s2.Stop()

	if jobs := s2.ListJobs("p", "c"); len(jobs) != 3 {
		t.Fatalf("MaxJobsPerChat=3 with 3 persisted: loaded %d, want 3", len(jobs))
	}
}
