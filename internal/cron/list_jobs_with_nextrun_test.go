package cron

import (
	"fmt"
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

// TestListJobsWithNextRun_LargeBucketMapPath drives the pooled-map branch
// (R20260602141221-PERF-2 / #1583): a chat with more than listNextRunMapThreshold
// jobs switches from the linear Entries() scan to the entryID→Next map. The
// result must be identical to what the linear path would produce — every active
// job gets its NextRun populated, paused jobs keep zero, and no foreign-chat job
// leaks in. Crossing the threshold (count > 8) guarantees both branches are
// exercised across the suite.
func TestListJobsWithNextRun_LargeBucketMapPath(t *testing.T) {
	t.Parallel()

	const n = listNextRunMapThreshold + 5 // strictly above threshold → map path
	s := NewScheduler(SchedulerConfig{MaxJobs: n + 5, MaxJobsPerChat: n + 5})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	ids := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		j := &Job{Schedule: "@hourly", Prompt: fmt.Sprintf("p%d", i), Platform: "p", ChatID: "big"}
		if err := s.AddJob(j); err != nil {
			t.Fatalf("AddJob[%d]: %v", i, err)
		}
		ids[j.ID] = true
	}
	// A foreign-chat job that must never leak into the "big" result.
	foreign := &Job{Schedule: "@hourly", Prompt: "f", Platform: "p", ChatID: "other"}
	if err := s.AddJob(foreign); err != nil {
		t.Fatalf("AddJob(foreign): %v", err)
	}

	got := s.ListJobsWithNextRun("p", "big")
	if len(got) != n {
		t.Fatalf("ListJobsWithNextRun(big) returned %d jobs, want %d", len(got), n)
	}
	for _, jw := range got {
		if jw.Job.ChatID != "big" {
			t.Fatalf("leaked job from chat %q", jw.Job.ChatID)
		}
		if !ids[jw.Job.ID] {
			t.Fatalf("unexpected job ID %q in result", jw.Job.ID)
		}
		if jw.NextRun.IsZero() {
			t.Errorf("job %s has zero NextRun via map path; want a scheduled time", jw.Job.ID)
		}
		if jw.Job.ID == foreign.ID {
			t.Fatalf("foreign-chat job leaked into big result")
		}
	}
}

// TestListJobsWithNextRun_PausedJobZeroNextRun pins that a paused (unregistered,
// entryID==0) job keeps a zero NextRun on BOTH the linear and the map branch.
func TestListJobsWithNextRun_PausedJobZeroNextRun(t *testing.T) {
	t.Parallel()

	s := NewScheduler(SchedulerConfig{MaxJobs: 4})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	active := &Job{Schedule: "@hourly", Prompt: "a", Platform: "p", ChatID: "c"}
	paused := &Job{Schedule: "@hourly", Prompt: "b", Platform: "p", ChatID: "c", Paused: true}
	for _, j := range []*Job{active, paused} {
		if err := s.AddJob(j); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
	}

	got := s.ListJobsWithNextRun("p", "c")
	if len(got) != 2 {
		t.Fatalf("got %d jobs, want 2", len(got))
	}
	for _, jw := range got {
		switch jw.Job.ID {
		case active.ID:
			if jw.NextRun.IsZero() {
				t.Errorf("active job has zero NextRun")
			}
		case paused.ID:
			if !jw.NextRun.IsZero() {
				t.Errorf("paused job has non-zero NextRun %v, want zero", jw.NextRun)
			}
		}
	}
}

// BenchmarkListJobsWithNextRun lets a maintainer confirm the threshold choice:
// run with -benchmem at a few bucket sizes (e.g. 1, 5, 8, 13) to see the linear
// path stay allocation-free below the threshold and the map path cap the
// comparison growth above it. Many other chats hold entries so |entries| is
// large regardless of the target bucket size. R20260602141221-PERF-2 (#1583).
func BenchmarkListJobsWithNextRun(b *testing.B) {
	for _, bucket := range []int{1, 5, listNextRunMapThreshold, listNextRunMapThreshold + 5} {
		bucket := bucket
		b.Run(fmt.Sprintf("bucket=%d", bucket), func(b *testing.B) {
			s := NewScheduler(SchedulerConfig{MaxJobs: 600, MaxJobsPerChat: 600})
			if err := s.Start(); err != nil {
				b.Fatalf("Start: %v", err)
			}
			defer s.Stop()
			// Pad many other chats so Entries() is large (the cost the inner
			// scan pays per job).
			for i := 0; i < 400; i++ {
				j := &Job{Schedule: "@hourly", Prompt: "pad", Platform: "pad", ChatID: fmt.Sprintf("chat%d", i)}
				if err := s.AddJob(j); err != nil {
					b.Fatalf("AddJob(pad): %v", err)
				}
			}
			for i := 0; i < bucket; i++ {
				j := &Job{Schedule: "@hourly", Prompt: "t", Platform: "p", ChatID: "target"}
				if err := s.AddJob(j); err != nil {
					b.Fatalf("AddJob(target): %v", err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = s.ListJobsWithNextRun("p", "target")
			}
		})
	}
}
