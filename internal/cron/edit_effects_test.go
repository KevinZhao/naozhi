package cron

import (
	"path/filepath"
	"slices"
	"testing"
)

// newEditEffectsScheduler has a store and a router that records stub
// registrations, with one active job holding a prompt and one paused job
// without.
func newEditEffectsScheduler(t *testing.T) (s *Scheduler, r *reapRouter, activeID, emptyID string) {
	t.Helper()
	r = &reapRouter{}
	s = NewScheduler(SchedulerConfig{StorePath: filepath.Join(t.TempDir(), "cron.json"), MaxJobs: 5}, SchedulerDeps{Router: r})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)
	active := &Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "c", ChatType: "direct"}
	empty := &Job{Schedule: "@every 1h", Platform: "feishu", ChatID: "c", ChatType: "direct", Paused: true}
	for _, j := range []*Job{active, empty} {
		if err := s.AddJob(j); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
	}
	return s, r, active.ID, empty.ID
}

func lastStub(t *testing.T, r *reapRouter) stubCall {
	t.Helper()
	_, regs := r.snapshot()
	if len(regs) == 0 {
		t.Fatal("no stub registered")
	}
	return regs[len(regs)-1]
}

// UpdateJob and SetJobPrompt each reach cron_jobs.json and refresh the
// router stub with what they wrote.
func TestUpdateJobAndSetJobPrompt_PersistAndRefreshStub(t *testing.T) {
	s, r, activeID, emptyID := newEditEffectsScheduler(t)
	onDisk := func(id string) *Job {
		t.Helper()
		jobs, err := loadJobs(s.storePath)
		if err != nil || jobs[id] == nil {
			t.Fatalf("loadJobs: %v, job %s present=%v", err, id, jobs[id] != nil)
		}
		return jobs[id]
	}
	// Each check reads disk before the next write: any later save carries the
	// whole job set and would hide a skipped one.
	edited := "edited"
	if _, err := s.UpdateJob(activeID, JobUpdate{Prompt: &edited}); err != nil {
		t.Fatal(err)
	}
	if j := onDisk(activeID); j.Prompt != "edited" {
		t.Errorf("UpdateJob on disk: prompt %q", j.Prompt)
	}
	if st := lastStub(t, r); st.prompt != "edited" {
		t.Errorf("UpdateJob stub prompt = %q", st.prompt)
	}
	if err := s.SetJobPrompt(emptyID, "filled"); err != nil {
		t.Fatal(err)
	}
	if j := onDisk(emptyID); j.Prompt != "filled" || j.Paused {
		t.Errorf("SetJobPrompt on disk: prompt %q paused %v", j.Prompt, j.Paused)
	}
	if st := lastStub(t, r); st.prompt != "filled" {
		t.Errorf("SetJobPrompt stub prompt = %q", st.prompt)
	}
}

// A reschedule's result is snapshotted before the entry commit; a session id a
// run records in between must still reach the caller and the stub, or the
// sidebar anchors on a stale session. Not parallel: cronCommitHook is
// package-level.
func TestUpdateJob_RescheduleCarriesSessionIDRecordedDuringCommit(t *testing.T) {
	s, r, activeID, _ := newEditEffectsScheduler(t)
	cronCommitHook = func() {
		s.tblForTest().mu.Lock()
		s.tblForTest().jobs[activeID].LastSessionID = "recorded-mid-commit"
		s.tblForTest().mu.Unlock()
	}
	t.Cleanup(func() { cronCommitHook = nil })

	next := "@every 2h"
	got, err := s.UpdateJob(activeID, JobUpdate{Schedule: &next})
	if err != nil {
		t.Fatal(err)
	}
	if got.LastSessionID != "recorded-mid-commit" {
		t.Errorf("result LastSessionID = %q, want the one recorded during the commit", got.LastSessionID)
	}
	if st := lastStub(t, r); !slices.Equal(st.chainIDs, []string{"recorded-mid-commit"}) {
		t.Errorf("stub chain = %v", st.chainIDs)
	}
}
