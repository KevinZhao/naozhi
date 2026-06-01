package cron

import (
	"testing"
	"time"
)

// TestListJobsWithNextRun pins the chat-narrowed list+NextRun helper added
// under R249-CR-12 (#956): it must return only the target chat's jobs (NOT the
// whole table like ListAllJobsWithNextRun), populate NextRun for registered
// jobs, and return a non-nil empty slice for an empty/unknown chat.
func TestListJobsWithNextRun(t *testing.T) {
	t.Parallel()

	s := NewScheduler(SchedulerConfig{MaxJobs: 10})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	jobA := &Job{Schedule: "@hourly", Prompt: "a", Platform: "p", ChatID: "c1"}
	jobB := &Job{Schedule: "@hourly", Prompt: "b", Platform: "p", ChatID: "c1"}
	jobOther := &Job{Schedule: "@hourly", Prompt: "x", Platform: "p", ChatID: "c2"}
	for _, j := range []*Job{jobA, jobB, jobOther} {
		if err := s.AddJob(j); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
	}

	// Chat scoping: c1 must yield exactly its two jobs, not jobOther.
	got := s.ListJobsWithNextRun("p", "c1")
	if len(got) != 2 {
		t.Fatalf("ListJobsWithNextRun(c1) returned %d jobs, want 2", len(got))
	}
	seen := map[string]bool{}
	for _, jw := range got {
		seen[jw.Job.ID] = true
		if jw.Job.ChatID != "c1" {
			t.Fatalf("leaked job from chat %q", jw.Job.ChatID)
		}
		// Active (non-paused) jobs are registered with cron → NextRun set.
		if jw.NextRun.IsZero() {
			t.Errorf("job %s has zero NextRun; want a scheduled time", jw.Job.ID)
		}
		if jw.NextRun.Before(time.Now()) {
			t.Errorf("job %s NextRun %v is in the past", jw.Job.ID, jw.NextRun)
		}
	}
	if !seen[jobA.ID] || !seen[jobB.ID] {
		t.Fatalf("missing expected job(s): seen=%v", seen)
	}
	if seen[jobOther.ID] {
		t.Fatalf("foreign-chat job %s leaked into c1 result", jobOther.ID)
	}

	// Empty/unknown chat: non-nil empty slice (wire-format symmetry with
	// ListJobs / ListAllJobsWithNextRun).
	empty := s.ListJobsWithNextRun("p", "no-such-chat")
	if empty == nil {
		t.Fatalf("ListJobsWithNextRun(unknown) returned nil, want non-nil empty slice")
	}
	if len(empty) != 0 {
		t.Fatalf("ListJobsWithNextRun(unknown) returned %d jobs, want 0", len(empty))
	}
}
