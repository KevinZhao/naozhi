package cron

import (
	"path/filepath"
	"testing"
)

// TestStart_CapsAllowExactlyAtCap pins the boundary of both load-time caps:
// when the on-disk count equals cron.MaxJobs or the per-chat cap, every job
// loads. The caps are "no MORE than", mirroring jobTable.insert's `>=`
// rejection. Over-cap loads are covered by TestStart_OverCapJobsSurviveSaves
// and TestStart_PerChatCapEnforced.
func TestStart_CapsAllowExactlyAtCap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		maxJobs, perChat int
	}{
		{"max_jobs", 3, 0},
		{"per_chat", 100, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "cron.json")
			s1 := NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: 100}, SchedulerDeps{})
			if err := s1.Start(); err != nil {
				t.Fatalf("phase 1 Start: %v", err)
			}
			for i := 0; i < 3; i++ {
				if err := s1.AddJob(&Job{Schedule: "@hourly", Prompt: "p", Platform: "p", ChatID: "c"}); err != nil {
					t.Fatalf("AddJob %d: %v", i, err)
				}
			}
			s1.Stop()

			s2 := NewScheduler(SchedulerConfig{StorePath: path, MaxJobs: tc.maxJobs}, SchedulerDeps{})
			if tc.perChat > 0 {
				s2.maxJobsPerChat = tc.perChat
			}
			if err := s2.Start(); err != nil {
				t.Fatalf("phase 2 Start: %v", err)
			}
			defer s2.Stop()
			if jobs := s2.ListJobs("p", "c"); len(jobs) != 3 {
				t.Fatalf("3 persisted at a cap of 3: loaded %d, want 3", len(jobs))
			}
		})
	}
}
