package cron

import (
	"path/filepath"
	"testing"
)

// schedulerForDeleteParityTest spins up a minimal scheduler whose runStore is
// enabled (StorePath has a sibling runs dir) so deleteJobPostCleanup's
// runStore.DeleteJob branch is actually exercised — not skipped by the
// enabled() gate.
func schedulerForDeleteParityTest(t *testing.T) *Scheduler {
	t.Helper()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath:      filepath.Join(dir, "cron.json"),
		MaxJobs:        8,
		AllowNilRouter: true,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })
	return s
}

// addParityJob inserts a job and returns it. Fails the test on any error so
// the parity assertions below operate on a known-good fixture.
func addParityJob(t *testing.T, s *Scheduler, title string) *Job {
	t.Helper()
	j := &Job{
		Schedule: "@every 1h",
		Title:    title,
		Prompt:   "noop",
		Platform: "feishu",
		ChatID:   "parity-chat",
	}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob(%s): %v", title, err)
	}
	if j.ID == "" {
		t.Fatalf("AddJob(%s) did not assign an ID", title)
	}
	return j
}

// TestDeletePostCleanup_ParityBetweenEntryPoints pins R244-ARCH-13 (#1053):
// the two delete entry points — DeleteJobByID (dashboard, exact ID) and
// DeleteJob (IM, ID-prefix scoped to chat) — must leave the scheduler in an
// identical post-delete state. Both now route their lock-free side effects
// through the shared deleteJobPostCleanup helper, so this test guards against
// a future change that re-diverges the two pipelines (e.g. adding a cleanup
// step to one entry point but not the other).
//
// The observable post-delete invariants checked for BOTH paths:
//   - the job is gone from s.jobs;
//   - the per-chat job count for the chat is reclaimed to 0;
//   - the per-chat job index slice for the chat is dropped;
//   - the runningJobs guard for the deleted ID is reclaimed (idle path).
func TestDeletePostCleanup_ParityBetweenEntryPoints(t *testing.T) {
	t.Parallel()

	type postState struct {
		jobPresent   bool
		chatCount    int
		chatIndexLen int
		runningGuard bool
	}

	capture := func(s *Scheduler, jobID string, key chatJobKey) postState {
		// runningJobs is a sync.Map accessed lock-free; everything else is
		// guarded by s.mu.
		_, runningGuard := s.runningJobs.Load(jobID)
		s.mu.RLock()
		defer s.mu.RUnlock()
		_, jobPresent := s.jobs[jobID]
		return postState{
			jobPresent:   jobPresent,
			chatCount:    s.chatJobCount[key],
			chatIndexLen: len(s.jobsByChat[key]),
			runningGuard: runningGuard,
		}
	}

	// Path A: DeleteJobByID.
	sA := schedulerForDeleteParityTest(t)
	jA := addParityJob(t, sA, "byid")
	keyA := chatJobKey{Platform: jA.Platform, ChatID: jA.ChatID}
	if _, err := sA.DeleteJobByID(jA.ID); err != nil {
		t.Fatalf("DeleteJobByID: %v", err)
	}
	stateA := capture(sA, jA.ID, keyA)

	// Path B: DeleteJob (prefix + plat + chat).
	sB := schedulerForDeleteParityTest(t)
	jB := addParityJob(t, sB, "prefix")
	keyB := chatJobKey{Platform: jB.Platform, ChatID: jB.ChatID}
	if _, err := sB.DeleteJob(jB.ID, jB.Platform, jB.ChatID); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	stateB := capture(sB, jB.ID, keyB)

	if stateA != stateB {
		t.Fatalf("delete post-state diverged between entry points:\n  DeleteJobByID=%+v\n  DeleteJob    =%+v\n(both must route through deleteJobPostCleanup — #1053)", stateA, stateB)
	}

	// Sanity: the shared state must actually reflect a fully cleaned delete,
	// not just two paths that are identically broken.
	want := postState{jobPresent: false, chatCount: 0, chatIndexLen: 0, runningGuard: false}
	if stateA != want {
		t.Fatalf("post-delete state = %+v; want %+v (job removed, chat index/count reclaimed, running guard reclaimed)", stateA, want)
	}
}
