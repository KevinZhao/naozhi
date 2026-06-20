package cron

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDeleteJobSandboxEvents_RemovesDir verifies that deleteJobSandboxEvents
// removes the sandboxevents/<jobID>/ subtree when persistence is enabled.
// R20260614-LOGIC-2: the helper must be called from deleteJobRuns so a
// deleted job leaves no orphaned event-log tree on disk.
func TestDeleteJobSandboxEvents_RemovesDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s := NewScheduler(SchedulerConfig{
		StorePath:      storePath,
		MaxJobs:        5,
		AllowNilRouter: true,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	const jobID = "0123456789abcdef"
	eventsDir := filepath.Join(dir, "sandboxevents", jobID)
	if err := os.MkdirAll(eventsDir, 0o700); err != nil {
		t.Fatalf("setup: MkdirAll: %v", err)
	}
	eventFile := filepath.Join(eventsDir, "abcdef0123456789.ndjson")
	if err := os.WriteFile(eventFile, []byte(`{"event":"start"}`+"\n"), 0o600); err != nil {
		t.Fatalf("setup: WriteFile: %v", err)
	}

	s.deleteJobSandboxEvents(jobID)

	if _, err := os.Stat(eventsDir); !os.IsNotExist(err) {
		t.Errorf("sandboxevents/<jobID>/ still exists after deleteJobSandboxEvents; stat err=%v", err)
	}
}

// TestDeleteJobSandboxEvents_MissingDirNoError verifies that
// deleteJobSandboxEvents is a no-op when the subtree does not exist —
// os.RemoveAll on a missing path returns nil.
func TestDeleteJobSandboxEvents_MissingDirNoError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s := NewScheduler(SchedulerConfig{
		StorePath:      storePath,
		MaxJobs:        5,
		AllowNilRouter: true,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	// Directory was never created; helper must not error.
	s.deleteJobSandboxEvents("0123456789abcdef")
}

// TestDeleteJobSandboxEvents_EmptyStorePathSkips verifies the storePath==""
// guard: when persistence is disabled the helper returns without touching
// the filesystem, mirroring the sandboxSnapshotDir empty-storePath guard.
func TestDeleteJobSandboxEvents_EmptyStorePathSkips(t *testing.T) {
	t.Parallel()

	s := NewScheduler(SchedulerConfig{
		MaxJobs:        5,
		AllowNilRouter: true,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	// Must not panic; no filesystem side-effects to assert on.
	s.deleteJobSandboxEvents("0123456789abcdef")
}

// TestDeleteJobSandboxEvents_InvalidIDSkips verifies the IsValidID guard:
// a non-hex jobID must not attempt a RemoveAll (path-traversal protection).
func TestDeleteJobSandboxEvents_InvalidIDSkips(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s := NewScheduler(SchedulerConfig{
		StorePath:      storePath,
		MaxJobs:        5,
		AllowNilRouter: true,
	}, SchedulerDeps{})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	// Plant a directory that would be at risk from a traversal.
	victim := filepath.Join(dir, "sandboxevents", "../canary")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatalf("setup: MkdirAll: %v", err)
	}

	// Non-hex ID should be rejected by IsValidID — no removal attempted.
	s.deleteJobSandboxEvents("../canary")

	if _, err := os.Stat(filepath.Join(dir, "canary")); os.IsNotExist(err) {
		t.Error("IsValidID guard failed: canary directory was removed by an invalid jobID")
	}
}

// TestDeleteJobByID_CleansSandboxEventsDir is the integration path:
// DeleteJobByID must call deleteJobSandboxEvents so a deleted job leaves
// no orphaned sandboxevents/<jobID>/ tree on disk.
func TestDeleteJobByID_CleansSandboxEventsDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s := NewScheduler(SchedulerConfig{
		StorePath: storePath,
		MaxJobs:   5,
	}, SchedulerDeps{Router: okRouter{}})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	job := &Job{Schedule: "@every 1h", Prompt: "p", Platform: "feishu", ChatID: "c", ChatType: "direct"}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Plant an event-log file as if a sandbox run had written it.
	eventsDir := filepath.Join(dir, "sandboxevents", job.ID)
	if err := os.MkdirAll(eventsDir, 0o700); err != nil {
		t.Fatalf("setup: MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(eventsDir, "runid0123456789ab.ndjson"), []byte(`{"e":"start"}`), 0o600); err != nil {
		t.Fatalf("setup: WriteFile: %v", err)
	}

	if _, err := s.DeleteJobByID(job.ID); err != nil {
		t.Fatalf("DeleteJobByID: %v", err)
	}

	if _, err := os.Stat(eventsDir); !os.IsNotExist(err) {
		t.Errorf("sandboxevents/<jobID>/ still exists after DeleteJobByID; stat err=%v", err)
	}
}
