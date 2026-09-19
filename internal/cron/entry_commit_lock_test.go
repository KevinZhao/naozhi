package cron

import (
	"errors"
	"path/filepath"
	"testing"
)

// newEntryLockScheduler builds a started scheduler with one active and one
// paused job, for the write-path exercises below. The hook is installed by the
// caller AFTER Start: Start's load loop is the one legitimate under-lock commit
// site (robfig is not running yet, Schedule appends to a slice), so it is
// exempt from the discipline by contract rather than by accident.
func newEntryLockScheduler(t *testing.T) (s *Scheduler, activeID, pausedID string) {
	t.Helper()
	dir := t.TempDir()
	s = NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   10,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)

	a := &Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "c1", ChatType: "direct"}
	if err := s.AddJob(a); err != nil {
		t.Fatalf("AddJob active: %v", err)
	}
	p := &Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "c1", ChatType: "direct", Paused: true}
	if err := s.AddJob(p); err != nil {
		t.Fatalf("AddJob paused: %v", err)
	}
	return s, a.ID, p.ID
}

// TestEntryCommitNeverUnderRegistryLock is the machine form of the rule the
// plan/commit/apply split exists for: no production writer commits a robfig
// entry while holding s.mu. It is the runtime stand-in for the go/analysis
// pass the RFC left as an open decision (§8.5) — cheaper, and it fails the
// moment a code path regresses instead of waiting for a linter to be written.
//
// Mechanism: cronCommitHook fires inside commitCronEntry, immediately before
// the robfig rendezvous. The test runs every write path SERIALLY, so at hook
// time the only goroutine that could hold s.mu is the one committing —
// TryLock failing therefore means self-deadlock-shaped code: a commit under
// the registry lock.
//
// Not t.Parallel, and the hook is package-level: parallel tests would make
// TryLock fail for innocent reasons.
func TestEntryCommitNeverUnderRegistryLock(t *testing.T) {
	s, activeID, pausedID := newEntryLockScheduler(t)

	commits := 0
	cronCommitHook = func() {
		commits++
		if !s.mu.TryLock() {
			t.Error("commitCronEntry reached with s.mu held: a robfig rendezvous under the registry lock")
			return
		}
		s.mu.Unlock()
	}
	t.Cleanup(func() { cronCommitHook = nil })

	// Every write path that ends in a commit, serially.
	newJob := &Job{Schedule: "@every 2h", Prompt: "p", Platform: "feishu", ChatID: "c2", ChatType: "direct"}
	if err := s.AddJob(newJob); err != nil { // AddJob (active)
		t.Fatalf("AddJob: %v", err)
	}
	if _, err := s.ResumeJobByID(pausedID); err != nil { // byid resume
		t.Fatalf("ResumeJobByID: %v", err)
	}
	if _, err := s.PauseJob(pausedID[:6], "feishu", "c1"); err != nil { // re-pause for prefix resume
		t.Fatalf("PauseJob: %v", err)
	}
	if _, err := s.ResumeJob(pausedID[:6], "feishu", "c1"); err != nil { // prefix resume
		t.Fatalf("ResumeJob: %v", err)
	}
	sched := "@every 3h"
	if _, err := s.UpdateJob(activeID, JobUpdate{Schedule: &sched}); err != nil { // update swap
		t.Fatalf("UpdateJob: %v", err)
	}
	// SetJobPrompt's resume path needs a paused, empty-prompt job.
	empty := &Job{Schedule: "@every 1h", Platform: "feishu", ChatID: "c3", ChatType: "direct", Paused: true}
	if err := s.AddJob(empty); err != nil {
		t.Fatalf("AddJob empty-prompt: %v", err)
	}
	if err := s.SetJobPrompt(empty.ID, "filled in"); err != nil { // prompt-fill resume
		t.Fatalf("SetJobPrompt: %v", err)
	}

	if commits != 5 {
		t.Errorf("hook saw %d commits, want 5 (AddJob, resume ×2, update, prompt-fill) — a path stopped committing or bypassed commitCronEntry", commits)
	}
}

// TestResumePersistFailure_LeavesNoEntry pins the property that dissolves
// #1226's rollback machinery: with the commit deferred until after persist, a
// persist failure means no robfig entry was ever created — nothing to remove,
// nothing to double-fire. The old order registered first, so this exact case
// once needed an entryID snapshot threaded through the caller and an
// out-of-lock Remove; the existing TestPersistFailure_ResumeJobByID checks
// only the returned error, which both orders satisfy.
func TestResumePersistFailure_LeavesNoEntry(t *testing.T) {
	s, _, pausedID := newEntryLockScheduler(t)
	entriesBefore := len(s.cron.Entries())

	withFailingMarshal(t, s)

	if _, err := s.ResumeJobByID(pausedID); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("ResumeJobByID err = %v, want ErrPersistFailed", err)
	}
	if n := len(s.cron.Entries()); n != entriesBefore {
		t.Errorf("robfig entries %d → %d after failed resume: an entry was committed before persist and leaked", entriesBefore, n)
	}
	if reg, paused := s.liveness(pausedID); !reg || !paused {
		t.Errorf("liveness = (%v, %v), want (true, true) — the rollback must restore Paused", reg, paused)
	}
}

// TestResume_RegistersEntryAndCache is the other direction: the deferred commit
// still happens, writes the entry id and the jitter/missed-schedule cache back,
// and the job fires on robfig's books exactly once.
func TestResume_RegistersEntryAndCache(t *testing.T) {
	s, _, pausedID := newEntryLockScheduler(t)
	entriesBefore := len(s.cron.Entries())

	got, err := s.ResumeJobByID(pausedID)
	if err != nil {
		t.Fatalf("ResumeJobByID: %v", err)
	}
	if got.Paused {
		t.Error("returned snapshot still Paused")
	}
	if n := len(s.cron.Entries()); n != entriesBefore+1 {
		t.Errorf("robfig entries %d → %d, want exactly one new entry", entriesBefore, n)
	}
	s.mu.RLock()
	j := s.jobs[pausedID]
	entryID, period, sched := j.entryID, j.cachedPeriod, j.cachedSched
	s.mu.RUnlock()
	if entryID == 0 {
		t.Error("entryID = 0 after resume: the commit never applied back")
	}
	if period <= 0 || sched == nil {
		t.Errorf("cache not populated after resume: period=%v sched=%v — jitter and HasMissedSchedule fall back to re-parsing", period, sched)
	}
}

// TestEntryLifecycle_ConcurrentWritersInvariant hammers the three entry writers
// against each other and asserts the one invariant they all exist to preserve:
// a job has exactly one live robfig entry when active and exactly zero when
// paused. This is what entryMu buys on the resume/pause paths — without it a
// pause landing inside resume's plan → commit window sees entryID=0, no-ops its
// Remove, and the deferred commit then revives an entry for a job that reads as
// paused (the inverse of #2760's double-entry, same window).
//
// Rounds rather than duration: #2760's variant of this race reproduced within
// ~50 rounds; 300 keeps the test under a few seconds while staying far above
// that. The invariant is checked between rounds, when no writer is in flight.
func TestEntryLifecycle_ConcurrentWritersInvariant(t *testing.T) {
	s, activeID, _ := newEntryLockScheduler(t)

	scheds := []string{"@every 2h", "@every 3h", "@every 4h", "@every 5h"}
	for round := 0; round < 300; round++ {
		ops := []func(){
			func() { _, _ = s.PauseJobByID(activeID) },
			func() { _, _ = s.ResumeJobByID(activeID) },
			func() { sc := scheds[round%len(scheds)]; _, _ = s.UpdateJob(activeID, JobUpdate{Schedule: &sc}) },
		}
		// Two concurrent writers per round, rotating the pairing.
		a, b := ops[round%3], ops[(round+1)%3]
		done := make(chan struct{}, 2)
		go func() { a(); done <- struct{}{} }()
		go func() { b(); done <- struct{}{} }()
		<-done
		<-done

		s.mu.RLock()
		paused := s.jobs[activeID].Paused
		s.mu.RUnlock()
		// The other seeded job is paused throughout, so it contributes 0.
		want := 1
		if paused {
			want = 0
		}
		if got := len(s.cron.Entries()); got != want {
			t.Fatalf("round %d: %d live entries with Paused=%v, want %d — a writer slipped into another's plan→commit window",
				round, got, paused, want)
		}
	}
}
