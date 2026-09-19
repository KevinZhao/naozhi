package cron

import (
	"path/filepath"
	"sync"
	"testing"
)

// TestUpdateJob_ConcurrentScheduleChangesKeepOneEntry pins the invariant a cron
// job has exactly one live robfig entry, across concurrent schedule edits.
//
// It failed on 02b33b69 at round 43 of 200: UpdateJob clears j.entryID under
// s.tbl.mu, releases the lock so the robfig Remove/Schedule rendezvous does not park
// registry readers, and re-registers in a second critical section. Two of those
// interleaved leave the loser's Remove looking at the zero id — a no-op — while
// both register, so both entries stay live. The job then fires on the union of
// the two schedules (more often than configured), and DeleteJob later removes
// only the one j.entryID names.
//
// 200 rounds because it is a timing race: a single round reproduced it maybe
// once in tens of runs. Counting robfig's entries is the assertion because it is
// the only place the second entry exists — j.entryID cannot show it, which is
// exactly why the bug was invisible.
func TestUpdateJob_ConcurrentScheduleChangesKeepOneEntry(t *testing.T) {
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	j := &Job{Schedule: "@hourly", Prompt: "p", Platform: "x", ChatID: "c1"}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	id := j.ID
	if n := len(s.cron.Entries()); n != 1 {
		t.Fatalf("after AddJob: entries = %d, want 1", n)
	}

	scheds := []string{"@daily", "@weekly", "@monthly", "@hourly"}
	for round := 0; round < 200; round++ {
		a := scheds[round%len(scheds)]
		b := scheds[(round+1)%len(scheds)]
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = s.UpdateJob(id, JobUpdate{Schedule: &a}) }()
		go func() { defer wg.Done(); _, _ = s.UpdateJob(id, JobUpdate{Schedule: &b}) }()
		wg.Wait()
		if n := len(s.cron.Entries()); n != 1 {
			t.Fatalf("round %d: robfig holds %d entries for one job — the job now fires on %d schedules", round, n, n)
		}
	}

	// And the entry is still the one the job names, so DeleteJob can remove it.
	if _, err := s.DeleteJobByID(id); err != nil {
		t.Fatalf("DeleteJobByID: %v", err)
	}
	if n := len(s.cron.Entries()); n != 0 {
		t.Errorf("after delete: entries = %d, want 0 — an orphan entry keeps ticking for a job that is gone", n)
	}
}
