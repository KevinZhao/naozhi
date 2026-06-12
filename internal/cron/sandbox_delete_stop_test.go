package cron

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestStopSandboxRunsForJob_StopsAndRemoves: deleting a job that has an
// in-flight sandbox run (pending record present) Stops the recorded runtime
// session and removes the pending file.
func TestStopSandboxRunsForJob_StopsAndRemoves(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, _ := sandboxTestScheduler(t, runner, storePath)

	path := writePendingFixture(t, storePath, sandboxPending{
		JobID: "0123456789abcdef", RunID: "feedfacefeedface",
		RuntimeSessionID: "run-feedfacefeedface-1234567890123456789",
		StartedAtMS:      time.Now().UnixMilli(),
	})

	s.stopSandboxRunsForJob("0123456789abcdef")

	runner.mu.Lock()
	stopped := append([]string(nil), runner.stopped...)
	runner.mu.Unlock()
	if len(stopped) != 1 || stopped[0] != "run-feedfacefeedface-1234567890123456789" {
		t.Fatalf("StopSession calls = %v, want the recorded runtime id", stopped)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pending file must be removed after confirmed Stop")
	}
}

// TestStopSandboxRunsForJob_StopFailureKeepsPending pins §6.2: a failed
// Stop leaves the pending record so startup reconcile retries — the
// microVM fate is unknown until a confirmed Stop.
func TestStopSandboxRunsForJob_StopFailureKeepsPending(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{stopErr: errors.New("api down")}
	s, _ := sandboxTestScheduler(t, runner, storePath)

	path := writePendingFixture(t, storePath, sandboxPending{
		JobID: "0123456789abcdef", RunID: "deadbeefdeadbeef",
		RuntimeSessionID: "run-deadbeefdeadbeef-1234567890123456789",
		StartedAtMS:      time.Now().UnixMilli(),
	})

	s.stopSandboxRunsForJob("0123456789abcdef")

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pending file must survive a failed Stop: %v", err)
	}
}

// TestStopSandboxRunsForJob_IgnoresOtherJobs: only the deleted job's
// pending record is touched; a sibling job's in-flight run is untouched.
func TestStopSandboxRunsForJob_IgnoresOtherJobs(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, _ := sandboxTestScheduler(t, runner, storePath)

	otherPath := writePendingFixture(t, storePath, sandboxPending{
		JobID: "aaaaaaaaaaaaaaaa", RunID: "1111111111111111",
		RuntimeSessionID: "run-1111111111111111-1234567890123456789",
		StartedAtMS:      time.Now().UnixMilli(),
	})

	s.stopSandboxRunsForJob("0123456789abcdef") // a different job

	runner.mu.Lock()
	n := len(runner.stopped)
	runner.mu.Unlock()
	if n != 0 {
		t.Fatalf("StopSession called %d times for a non-matching job, want 0", n)
	}
	if _, err := os.Stat(otherPath); err != nil {
		t.Fatal("sibling job's pending record must be untouched")
	}
}

// TestStopSandboxRunsForJob_NoSandboxNoOp: with sandbox unconfigured the
// helper is a no-op (no pending records can exist for a sandbox run).
func TestStopSandboxRunsForJob_NoSandboxNoOp(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, _ := sandboxTestScheduler(t, nil, storePath)
	// Even if a stale pending file exists, nil sandbox short-circuits.
	writePendingFixture(t, storePath, sandboxPending{
		JobID: "0123456789abcdef", RunID: "2222222222222222",
		RuntimeSessionID: "run-2222222222222222-1234567890123456789",
		StartedAtMS:      time.Now().UnixMilli(),
	})
	s.stopSandboxRunsForJob("0123456789abcdef") // must not panic / act
}

// TestDeleteJobByID_StopsInflightSandbox is the integration path: a full
// DeleteJobByID drives deleteJobPostCleanup → stopSandboxRunsForJob. Uses
// okRouter (not the panicRouter sandboxTestScheduler installs) because the
// delete path legitimately calls router.Reset to tear down the sidebar
// stub — that is unrelated to the sandbox-run Stop under test.
func TestDeleteJobByID_StopsInflightSandbox(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: storePath},
		SchedulerDeps{Router: okRouter{}, Sandbox: runner})
	t.Cleanup(func() { s.Stop() })
	j := sandboxJob(t, s)

	path := writePendingFixture(t, storePath, sandboxPending{
		JobID: j.ID, RunID: "cafecafecafecafe",
		RuntimeSessionID: "run-cafecafecafecafe-1234567890123456789",
		StartedAtMS:      time.Now().UnixMilli(),
	})

	if _, err := s.DeleteJobByID(j.ID); err != nil {
		t.Fatalf("DeleteJobByID: %v", err)
	}

	runner.mu.Lock()
	n := len(runner.stopped)
	runner.mu.Unlock()
	if n != 1 {
		t.Fatalf("delete must Stop the in-flight microVM; StopSession calls = %d", n)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pending file must be removed after delete-stop")
	}
}
