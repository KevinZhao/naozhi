package cron

import (
	"testing"
)

// revertReschedule is UpdateJob's way back when the new schedule fails to
// plan after the old entry is already gone. validateSchedule shares the
// parser, so the public API cannot reach it; these drive it directly.
//
// When even the old schedule no longer plans, the job must end Paused — on
// disk too — so the dashboard shows the degraded state instead of an active
// job with entryID=0 that never fires (R20260607-LOGIC-1).
func TestRevertReschedule_OldScheduleBroken_JobMarkedPaused(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	if _, err := s.ResumeJobByID(id); err != nil {
		t.Fatal(err)
	}
	entriesBefore := len(s.cron.Entries())

	s.revertReschedule(id, "INVALID_FOR_REREG")

	j, _ := s.tbl.snapshot(id)
	if !j.Paused || j.Schedule != "INVALID_FOR_REREG" {
		t.Errorf("live job = paused %v schedule %q, want paused on the old schedule", j.Paused, j.Schedule)
	}
	if onDisk, err := loadJobs(s.storePath); err != nil || onDisk[id] == nil || !onDisk[id].Paused {
		t.Errorf("on disk: %+v, err %v; want paused", onDisk[id], err)
	}
	if n := len(s.cron.Entries()); n != entriesBefore {
		t.Errorf("entries %d → %d; a broken schedule must not register", entriesBefore, n)
	}
}

// When the old schedule still plans, the job goes back to it with a live
// entry, and disk agrees.
func TestRevertReschedule_RestoresOldEntry(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	if _, err := s.ResumeJobByID(id); err != nil {
		t.Fatal(err)
	}
	// The state UpdateJob leaves before the new plan fails: the job on the new
	// schedule, its old entry retired.
	next := "@every 2h"
	r, err := s.tbl.update(id, JobUpdate{Schedule: &next})
	if err != nil || r.resched == nil {
		t.Fatalf("update: %+v, %v", r, err)
	}
	s.cron.Remove(r.resched.removeEntry)

	s.revertReschedule(id, r.resched.oldSchedule)

	j, _ := s.tbl.snapshot(id)
	if j.Paused || j.Schedule != "@every 1h" || s.NextRun(&j).IsZero() {
		t.Errorf("live job = paused %v schedule %q next %v, want active on @every 1h", j.Paused, j.Schedule, s.NextRun(&j))
	}
	if onDisk, err := loadJobs(s.storePath); err != nil || onDisk[id] == nil || onDisk[id].Schedule != "@every 1h" {
		t.Errorf("on disk: %+v, err %v; want @every 1h", onDisk[id], err)
	}
}

// A job deleted before the revert lands is left alone, and the entry the
// revert committed for it is retired.
func TestRevertReschedule_JobGone(t *testing.T) {
	s, id := newTestSchedulerForPersist(t)
	if _, err := s.DeleteJobByID(id); err != nil {
		t.Fatal(err)
	}
	before := len(s.cron.Entries())
	s.revertReschedule(id, "@every 1h")
	if _, ok := s.tbl.snapshot(id); ok {
		t.Error("revert resurrected a deleted job")
	}
	if n := len(s.cron.Entries()); n != before {
		t.Errorf("entries %d → %d; the orphan entry must be retired", before, n)
	}
}
