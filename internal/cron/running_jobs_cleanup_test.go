// running_jobs_cleanup_test.go pins R242-ARCH-15 (#758): the
// runningJobs sync.Map entry for a job is reclaimed when DeleteJob runs
// AND the CAS gate is idle. The prior policy left every entry pinned
// for the process lifetime, leaking ~one *runInflight per historical
// jobID — bounded by maxJobsHardCap=500 in the steady-state, but
// unbounded over long deployments that delete and re-add jobs.
package cron

import (
	"path/filepath"
	"testing"
)

// TestRunningJobsCleanup_DeleteJobByIDClearsIdle verifies the happy
// path: a job with no in-flight execute() loses its runningJobs entry
// on DeleteJobByID.
func TestRunningJobsCleanup_DeleteJobByIDClearsIdle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   10,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	j := &Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "X"}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	// Touch jobInflight so the entry exists in s.runningJobs.
	_ = s.jobInflight(j.ID)
	if _, present := s.runningJobs.Load(j.ID); !present {
		t.Fatalf("precondition: runningJobs entry should exist after jobInflight touch")
	}

	if _, err := s.DeleteJobByID(j.ID); err != nil {
		t.Fatalf("DeleteJobByID: %v", err)
	}
	if _, present := s.runningJobs.Load(j.ID); present {
		t.Errorf("runningJobs entry should be cleaned up after DeleteJobByID on idle job")
	}
}

// TestRunningJobsCleanup_DeleteJobPrefixClearsIdle covers the IM-prefix
// DeleteJob path. Both delete entry points must reclaim the entry —
// otherwise the CLI / IM op route never frees memory.
func TestRunningJobsCleanup_DeleteJobPrefixClearsIdle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   10,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	j := &Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "Y"}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	_ = s.jobInflight(j.ID)
	if _, present := s.runningJobs.Load(j.ID); !present {
		t.Fatalf("precondition: runningJobs entry should exist")
	}
	// DeleteJob takes a prefix — feed the full ID.
	if _, err := s.DeleteJob(j.ID, "feishu", "Y"); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	if _, present := s.runningJobs.Load(j.ID); present {
		t.Errorf("runningJobs entry should be cleaned up after prefix DeleteJob")
	}
}

// TestRunningJobsCleanup_RetainsBusyEntry verifies the safety guard:
// when a runInflight is currently held (running.Load() == true), the
// entry stays put so the CAS gate is not split between two goroutines
// in the (vanishingly rare) ID-reuse window.
func TestRunningJobsCleanup_RetainsBusyEntry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   10,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	j := &Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "Z"}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	guard := s.jobInflight(j.ID)
	// Simulate "execute() in flight" by holding the CAS gate.
	if !guard.running.CompareAndSwap(false, true) {
		t.Fatalf("precondition: guard CAS should succeed on a fresh runInflight")
	}
	t.Cleanup(func() { guard.running.Store(false) })

	deleted := s.cleanupRunningJobIfIdle(j.ID)
	if deleted {
		t.Errorf("cleanupRunningJobIfIdle should NOT delete a busy entry")
	}
	if _, present := s.runningJobs.Load(j.ID); !present {
		t.Errorf("runningJobs entry should remain while busy")
	}

	// Releasing the gate makes the entry eligible; the next cleanup call wins.
	guard.running.Store(false)
	if !s.cleanupRunningJobIfIdle(j.ID) {
		t.Errorf("cleanupRunningJobIfIdle should delete after gate is released")
	}
	if _, present := s.runningJobs.Load(j.ID); present {
		t.Errorf("runningJobs entry should be deleted after release")
	}
}

// TestRunningJobsCleanup_NoEntryNoOp verifies the helper is safe to
// call when the map has no entry for the given jobID (e.g. a job that
// never ticked + never had jobInflight touched). Returns false, no
// panic.
func TestRunningJobsCleanup_NoEntryNoOp(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	if s.cleanupRunningJobIfIdle("nonexistent-id") {
		t.Errorf("cleanupRunningJobIfIdle on missing id should return false")
	}
}
