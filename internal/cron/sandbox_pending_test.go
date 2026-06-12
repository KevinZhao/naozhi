package cron

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pendingDirOf(storePath string) string {
	return filepath.Join(filepath.Dir(storePath), "sandboxpending")
}

func writePendingFixture(t *testing.T, storePath string, p sandboxPending) string {
	t.Helper()
	dir := pendingDirOf(storePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	b, _ := json.Marshal(p)
	path := filepath.Join(dir, p.RunID+".json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write pending: %v", err)
	}
	return path
}

// TestSandboxPending_WrittenAndRemovedAroundRun pins the §6.5 lifecycle:
// the pending record exists DURING the run (observed from inside the fake
// runner) and is gone after terminal state.
func TestSandboxPending_WrittenAndRemovedAroundRun(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")

	var seenDuringRun []string
	runner := &fakeSandboxRunner{outcome: SandboxOutcome{State: SandboxStateSuccess}}
	probe := &probeRunner{inner: runner, onRun: func(job SandboxJob) {
		entries, _ := os.ReadDir(pendingDirOf(storePath))
		for _, e := range entries {
			seenDuringRun = append(seenDuringRun, e.Name())
		}
		if job.RuntimeSessionID == "" {
			t.Error("RuntimeSessionID must be derived before RunJob")
		}
		if len(job.RuntimeSessionID) < 33 {
			t.Errorf("RuntimeSessionID %q shorter than API minimum", job.RuntimeSessionID)
		}
		if !strings.Contains(job.RuntimeSessionID, job.RunID) {
			t.Errorf("RuntimeSessionID %q must embed cron runID %q for correlation", job.RuntimeSessionID, job.RunID)
		}
	}}
	s, rec := sandboxTestScheduler(t, probe, storePath)
	j := sandboxJob(t, s)

	s.executeOpt(j, true)
	waitEnded(t, rec)

	if len(seenDuringRun) != 1 {
		t.Fatalf("pending files during run = %v, want exactly 1", seenDuringRun)
	}
	left, _ := os.ReadDir(pendingDirOf(storePath))
	if len(left) != 0 {
		t.Fatalf("pending files after terminal = %d, want 0", len(left))
	}
}

// probeRunner wraps a SandboxRunner with an onRun hook.
type probeRunner struct {
	inner SandboxRunner
	onRun func(SandboxJob)
}

func (p *probeRunner) RunJob(ctx context.Context, job SandboxJob, sink func([]byte) error) (SandboxOutcome, error) {
	if p.onRun != nil {
		p.onRun(job)
	}
	return p.inner.RunJob(ctx, job, sink)
}

func (p *probeRunner) StopSession(ctx context.Context, id string) error {
	return p.inner.StopSession(ctx, id)
}

// TestSandboxReconcile_StopsOrphanAndClosesRun pins the startup pass: a
// leftover pending file → StopSession on the recorded runtime id → a
// failed-transport terminal record → file removed.
func TestSandboxReconcile_StopsOrphanAndClosesRun(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sandboxJob(t, s)

	path := writePendingFixture(t, storePath, sandboxPending{
		JobID: j.ID, RunID: "feedfacefeedface",
		RuntimeSessionID: "run-feedfacefeedface-1234567890123456789",
		StartedAtMS:      time.Now().Add(-5 * time.Minute).UnixMilli(),
	})

	s.reconcileSandboxPending()

	runner.mu.Lock()
	stopped := append([]string(nil), runner.stopped...)
	runner.mu.Unlock()
	if len(stopped) != 1 || stopped[0] != "run-feedfacefeedface-1234567890123456789" {
		t.Fatalf("StopSession calls = %v, want the recorded runtime id", stopped)
	}
	waitEnded(t, rec)
	ev := rec.endedAtCron(0)
	if ev.RunID != "feedfacefeedface" {
		t.Fatalf("terminal run id = %q", ev.RunID)
	}
	if ev.ErrorClass != ErrClassSandboxTransport {
		t.Fatalf("error_class = %q, want sandbox_transport", ev.ErrorClass)
	}
	if !strings.Contains(ev.ErrorMsg, "restarted") {
		t.Fatalf("errMsg %q must explain the restart orphan", ev.ErrorMsg)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pending file must be removed after reconcile")
	}
}

// TestSandboxReconcile_StopFailureKeepsPending pins §6.2: until Stop is
// confirmed the microVM's fate is unknown — the pending record must
// survive so the NEXT start retries.
func TestSandboxReconcile_StopFailureKeepsPending(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{stopErr: errors.New("api down")}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sandboxJob(t, s)

	path := writePendingFixture(t, storePath, sandboxPending{
		JobID: j.ID, RunID: "deadbeefdeadbeef",
		RuntimeSessionID: "run-deadbeefdeadbeef-1234567890123456789",
		StartedAtMS:      time.Now().UnixMilli(),
	})

	s.reconcileSandboxPending()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pending file must survive a failed Stop: %v", err)
	}
	if rec.endedCount() != 0 {
		t.Fatal("no terminal record before Stop confirms (§6.2)")
	}
}

// TestSandboxReconcile_DeletedJobClosesFileOnly: the orphan's job was
// deleted while naozhi was down — Stop still fires, no terminal broadcast
// (no job to attach it to), file removed.
func TestSandboxReconcile_DeletedJobClosesFileOnly(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, rec := sandboxTestScheduler(t, runner, storePath)

	path := writePendingFixture(t, storePath, sandboxPending{
		JobID: "0123456789abcdef", RunID: "cafebabecafebabe",
		RuntimeSessionID: "run-cafebabecafebabe-1234567890123456789",
		StartedAtMS:      time.Now().UnixMilli(),
	})

	s.reconcileSandboxPending()

	runner.mu.Lock()
	nStopped := len(runner.stopped)
	runner.mu.Unlock()
	if nStopped != 1 {
		t.Fatalf("StopSession calls = %d, want 1 (orphan microVM must die even if job is gone)", nStopped)
	}
	if rec.endedCount() != 0 {
		t.Fatal("no broadcast for a job that no longer exists")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pending file must be removed")
	}
}

// TestSandboxReconcile_CorruptRecordDropped: unparseable pending files are
// removed (cannot Stop what cannot be identified) instead of re-warning
// forever.
func TestSandboxReconcile_CorruptRecordDropped(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, _ := sandboxTestScheduler(t, runner, storePath)

	pdir := pendingDirOf(storePath)
	if err := os.MkdirAll(pdir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(pdir, "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	s.reconcileSandboxPending()

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("corrupt pending record must be dropped")
	}
}


// TestSandboxReconcile_NilSandboxKeepsPending pins review §6.5 F1: with the
// sandbox config absent at reconcile, the Stop primitive does not exist —
// the retry handle must survive until a boot where it does.
func TestSandboxReconcile_NilSandboxKeepsPending(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	s, rec := sandboxTestScheduler(t, nil, storePath)
	j := sandboxJob(t, s)

	path := writePendingFixture(t, storePath, sandboxPending{
		JobID: j.ID, RunID: "0011223344556677",
		RuntimeSessionID: "run-0011223344556677-1234567890123456789",
		StartedAtMS:      time.Now().UnixMilli(),
	})

	s.reconcileSandboxPending()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pending must survive nil-sandbox reconcile: %v", err)
	}
	if rec.endedCount() != 0 {
		t.Fatal("no terminal record without a confirmed Stop")
	}
}


// TestSandboxPending_KeptOnUnconfirmedTransport pins review §6.5 F2: an
// in-process transport failure whose Stop did NOT confirm must leave the
// pending file for startup reconcile — removing it would permanently
// discard the §6.2 retry handle.
func TestSandboxPending_KeptOnUnconfirmedTransport(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{
		outcome: SandboxOutcome{State: SandboxStateFailedTransport, ErrMsg: "reset", StopConfirmed: false},
	}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sandboxJob(t, s)

	s.executeOpt(j, true)
	waitEnded(t, rec)

	left, _ := os.ReadDir(pendingDirOf(storePath))
	if len(left) != 1 {
		t.Fatalf("pending files after unconfirmed transport = %d, want 1 (retry handle)", len(left))
	}
}

// TestSandboxPending_RemovedOnConfirmedTransport: Stop confirmed in-process
// spends the retry handle — no stale file for reconcile to chew on.
func TestSandboxPending_RemovedOnConfirmedTransport(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{
		outcome: SandboxOutcome{State: SandboxStateFailedTransport, ErrMsg: "reset", StopConfirmed: true},
	}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sandboxJob(t, s)

	s.executeOpt(j, true)
	waitEnded(t, rec)

	left, _ := os.ReadDir(pendingDirOf(storePath))
	if len(left) != 0 {
		t.Fatalf("pending files after confirmed transport = %d, want 0", len(left))
	}
}
