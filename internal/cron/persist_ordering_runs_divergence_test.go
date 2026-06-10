package cron

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPersistOrdering_RunsNeverDivergeAheadOfJob pins R249-ARCH-28 (#992):
// the two-step disk write in finishRun (cron_jobs.json Job fields THEN
// runs/<jobID>/<runID>.json) is not transactional, but the divergence is
// clamped to a single safe direction by the jobPersistOK gate.
//
// When the Job-side persist fails (here: injected marshal failure), the
// run-history Append MUST NOT execute. Otherwise a crash could leave a
// runs/ record with no corresponding Job-side counter — the under-report
// direction this gate exists to forbid. The complementary over-report
// direction (Job persisted, runs Append not yet flushed) is the only
// divergence the design tolerates because it self-heals on the next run.
//
// The test drives finishRun directly with a failing marshaler installed
// and asserts the runs/ tree stays empty.
func TestPersistOrdering_RunsNeverDivergeAheadOfJob(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron.json")
	s := NewScheduler(SchedulerConfig{
		StorePath: storePath,
		MaxJobs:   5,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)

	if s.runStore == nil || s.runStore.disabled {
		t.Fatal("runStore must be enabled for this test (StorePath set)")
	}

	j := &Job{
		Schedule: "@every 1h",
		Prompt:   "ping",
		Platform: "feishu",
		ChatID:   "chat1",
		ChatType: "direct",
		Paused:   true, // avoid registering a live cron entry
	}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	runsRoot := filepath.Join(dir, "runs")

	// Install the Job-persist failure AFTER the seed AddJob succeeded, so the
	// failure isolates the finishRun persist step under test.
	withFailingMarshal(t, s)

	inflight := s.jobInflight(j.ID)
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("initial CAS must succeed")
	}
	finalizer := &runFinalizer{inflight: inflight}

	s.finishRun(finishArgs{
		job:       j,
		runID:     "0123456789abcdef", // valid 16-hex so Append would NOT bail on id check
		startedAt: time.Now(),
		trigger:   TriggerScheduled,
		state:     RunStateSucceeded,
		sessionID: "sess-1",
		result:    "ok",
		finalizer: finalizer,
	})

	// jobPersistOK was false (marshal injected an error), so runStore.Append
	// must have been gated out: no per-job run directory, no run record.
	jobRunDir := filepath.Join(runsRoot, j.ID)
	if entries, err := os.ReadDir(jobRunDir); err == nil && len(entries) > 0 {
		t.Fatalf("runs/ diverged ahead of Job persist: found %d run record(s) in %s "+
			"despite jobPersistOK=false (Append must be gated by jobPersistOK)",
			len(entries), jobRunDir)
	}
}
