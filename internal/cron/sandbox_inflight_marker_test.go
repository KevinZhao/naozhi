package cron

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cron/sandboxstore"
)

// A sandbox run has sandboxpending/<runID>.json and the sandbox reconciler; the
// local runinflight marker must not be written for it as well, or two
// reconcilers that know nothing of each other settle the same run (#2970).
// Observed from inside the fake runner: the only moment either file exists.
func TestSandboxRun_WritesNoLocalInflightMarker(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")

	var markersDuringRun []string
	runner := &fakeSandboxRunner{outcome: SandboxOutcome{State: SandboxStateSuccess}}
	var s *Scheduler
	probe := &probeRunner{inner: runner, onRun: func(SandboxJob) {
		markersDuringRun = markerFiles(t, s)
		if left, _ := os.ReadDir(pendingDirOf(storePath)); len(left) != 1 {
			t.Errorf("pending files during run = %d, want 1 (the sandbox file is the run's in-flight record)", len(left))
		}
	}}
	s, rec := sandboxTestScheduler(t, probe, storePath)
	j := sandboxJob(t, s)

	s.executeOpt(j.ID, true)
	waitEnded(t, rec)

	if len(markersDuringRun) != 0 {
		t.Fatalf("runinflight markers during a sandbox run = %v, want none", markersDuringRun)
	}
}

// Both files for one runID on disk — what a binary before this fix left when it
// died mid sandbox run. claimRunInflight runs synchronously before cron.Start
// and used to record the run interrupted at once; the sandbox pass then found
// EndedAt set and only deleted its file, so the run showed as "naozhi restarted
// while this run was in flight" instead of a stopped microVM, with no
// sandbox_transport classification and no attention card. The marker must be
// yielded to the sandbox reconciler: one record, sandbox-classified, and the
// runtime session stopped.
func TestReconcile_SandboxPendingOwnsARunOverTheLocalMarker(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sandboxJob(t, s)

	const runID = "feedfacefeedface"
	startedAt := time.Now().Add(-5 * time.Minute)
	pending := writePendingFixture(t, storePath, sandboxstore.Pending{
		JobID: j.ID, RunID: runID,
		RuntimeSessionID: "run-feedfacefeedface-1234567890123456789",
		StartedAtMS:      startedAt.UnixMilli(),
	})
	marker := filepath.Join(s.runMarkers().Dir(), runID+".json")
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte(`{"job_id":"`+j.ID+`","run_id":"`+runID+`","started_at_ms":`+
		strconv.FormatInt(startedAt.UnixMilli(), 10)+`,"prompt":"do the thing"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// The real Start order: the local claim first, then the sandbox pass.
	s.settleRunInflight(s.claimRunInflight())
	s.reconcileSandboxPending()
	waitEnded(t, rec)

	if _, err := os.Stat(marker); err == nil {
		t.Error("runinflight marker still on disk; the next boot would settle it again")
	}
	if _, err := os.Stat(pending); err == nil {
		t.Error("sandbox pending file still on disk")
	}
	runner.mu.Lock()
	stopped := len(runner.stopped)
	runner.mu.Unlock()
	if stopped != 1 {
		t.Errorf("StopSession calls = %d, want 1: only the sandbox reconciler stops the microVM", stopped)
	}
	if n := rec.endedCount(); n != 1 {
		t.Fatalf("terminal events = %d, want exactly 1 (a double finish double-counts metrics)", n)
	}
	ev := rec.endedAtCron(0)
	if ev.RunID != runID {
		t.Fatalf("terminal run id = %q", ev.RunID)
	}
	if ev.ErrorClass != ErrClassSandboxTransport {
		t.Errorf("error_class = %q, want %q: the local reconciler must not classify a sandbox orphan", ev.ErrorClass, ErrClassSandboxTransport)
	}
	if runs := s.ListRuns(j.ID, 100, time.Time{}); len(runs) != 1 {
		t.Errorf("history rows = %d, want 1", len(runs))
	}
}
