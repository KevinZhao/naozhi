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

// TestSandboxReconcile_NoDoubleFinishForInProcessTerminal pins #2054: a
// transport-failed (StopConfirmed==false) run finishes ONCE in-process
// (addRun + metrics + durable runs/{jobID}/{runID}.json) while KEEPING its
// pending file for the §6.2 retry. The next startup's reconcile must NOT
// re-finish that already-terminal runID — doing so doubled RunCounters and
// emitted a phantom started→ended lifecycle to subscribers.
func TestSandboxReconcile_NoDoubleFinishForInProcessTerminal(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{
		outcome: SandboxOutcome{State: SandboxStateFailedTransport, ErrMsg: "reset", StopConfirmed: false},
	}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sandboxJob(t, s)

	// In-process transport failure: finishRun runs once, pending file kept.
	s.executeOpt(j, true)
	waitEnded(t, rec)

	startedAfterRun := rec.startedCount()
	endedAfterRun := rec.endedCount()
	s.mu.RLock()
	countersAfterRun := s.jobs[j.ID].RunCounters
	s.mu.RUnlock()
	if countersAfterRun.Total != 1 {
		t.Fatalf("RunCounters.Total after run = %d, want 1", countersAfterRun.Total)
	}

	left, _ := os.ReadDir(pendingDirOf(storePath))
	if len(left) != 1 {
		t.Fatalf("pending files after unconfirmed transport = %d, want 1 (retry handle)", len(left))
	}
	pendingPath := filepath.Join(pendingDirOf(storePath), left[0].Name())

	// Simulate the next startup reconcile. With the fix, the run is already
	// terminal on disk → only the microVM Stop + pending removal are owed;
	// finishRun must NOT fire a second time.
	s.reconcileSandboxPending()

	runner.mu.Lock()
	nStopped := len(runner.stopped)
	runner.mu.Unlock()
	if nStopped != 1 {
		t.Fatalf("StopSession calls = %d, want 1 (reconcile must still terminate the microVM)", nStopped)
	}
	if got := rec.startedCount(); got != startedAfterRun {
		t.Fatalf("RunStarted count grew %d→%d across reconcile — phantom lifecycle (#2054)", startedAfterRun, got)
	}
	if got := rec.endedCount(); got != endedAfterRun {
		t.Fatalf("RunEnded count grew %d→%d across reconcile — duplicate finish (#2054)", endedAfterRun, got)
	}
	s.mu.RLock()
	countersAfterReconcile := s.jobs[j.ID].RunCounters
	s.mu.RUnlock()
	if countersAfterReconcile.Total != 1 {
		t.Fatalf("RunCounters.Total after reconcile = %d, want 1 (durable counter must not double-count #2054)", countersAfterReconcile.Total)
	}
	if countersAfterReconcile != countersAfterRun {
		t.Fatalf("RunCounters changed across reconcile: %+v → %+v (#2054)", countersAfterRun, countersAfterReconcile)
	}
	if _, err := os.Stat(pendingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pending file must be removed after reconcile confirms the Stop (#2054)")
	}
}

// TestSandboxReconcile_TransientReadKeepsPendingNoDoubleFinish pins #2149: the
// dedup guard at sandbox_pending.go fires only on err==nil. When the run is
// ALREADY terminal in-process (durable record + kept pending file, the #2054
// scenario) but the reconcile-time read of that record hits a *transient* error
// (EACCES from the brief post-upgrade -rw------- window, or EIO/ESTALE on a
// FUSE/NFS backend), s.Run returns a non-nil error that is neither
// fs.ErrNotExist nor ErrCorruptRun. Pre-fix this fell through to a second
// finishRun, double-counting the durable RunCounters and emitting a phantom
// started→ended lifecycle. The fix treats such "fate unknown" reads
// conservatively: keep the pending file, do NOT re-finish, retry next start.
func TestSandboxReconcile_TransientReadKeepsPendingNoDoubleFinish(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{
		outcome: SandboxOutcome{State: SandboxStateFailedTransport, ErrMsg: "reset", StopConfirmed: false},
	}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sandboxJob(t, s)

	// In-process transport failure: finishRun runs once, pending file kept.
	s.executeOpt(j, true)
	waitEnded(t, rec)

	startedAfterRun := rec.startedCount()
	endedAfterRun := rec.endedCount()
	s.mu.RLock()
	countersAfterRun := s.jobs[j.ID].RunCounters
	s.mu.RUnlock()
	if countersAfterRun.Total != 1 {
		t.Fatalf("RunCounters.Total after run = %d, want 1", countersAfterRun.Total)
	}

	left, _ := os.ReadDir(pendingDirOf(storePath))
	if len(left) != 1 {
		t.Fatalf("pending files after unconfirmed transport = %d, want 1 (retry handle)", len(left))
	}
	pendingPath := filepath.Join(pendingDirOf(storePath), left[0].Name())

	// Make the terminal runs/<jobID>/<runID>.json record unreadable so the
	// reconcile-time s.Run read returns EACCES (a transient, non-ErrNotExist /
	// non-ErrCorruptRun error). Run records live under <dir>/runs/<jobID>/.
	runsDir := filepath.Join(dir, "runs", j.ID)
	recEntries, err := os.ReadDir(runsDir)
	if err != nil {
		t.Fatalf("ReadDir runs: %v", err)
	}
	if len(recEntries) != 1 {
		t.Fatalf("terminal records = %d, want 1", len(recEntries))
	}
	recPath := filepath.Join(runsDir, recEntries[0].Name())
	if err := os.Chmod(recPath, 0o000); err != nil {
		t.Fatalf("chmod terminal record: %v", err)
	}
	// Verify the read actually fails for this uid (root would still read it).
	if _, rerr := os.ReadFile(recPath); rerr == nil {
		t.Skip("cannot induce EACCES on this platform/uid; skipping transient-read test")
	}
	t.Cleanup(func() { _ = os.Chmod(recPath, 0o600) })

	// Reconcile: transient read error → keep pending, no second finish.
	s.reconcileSandboxPending()

	runner.mu.Lock()
	nStopped := len(runner.stopped)
	runner.mu.Unlock()
	if nStopped != 1 {
		t.Fatalf("StopSession calls = %d, want 1 (reconcile still terminates the microVM)", nStopped)
	}
	if got := rec.startedCount(); got != startedAfterRun {
		t.Fatalf("RunStarted count grew %d→%d across reconcile — phantom lifecycle (#2149)", startedAfterRun, got)
	}
	if got := rec.endedCount(); got != endedAfterRun {
		t.Fatalf("RunEnded count grew %d→%d across reconcile — duplicate finish (#2149)", endedAfterRun, got)
	}
	s.mu.RLock()
	countersAfterReconcile := s.jobs[j.ID].RunCounters
	s.mu.RUnlock()
	if countersAfterReconcile != countersAfterRun {
		t.Fatalf("RunCounters changed across reconcile under transient read: %+v → %+v (#2149)", countersAfterRun, countersAfterReconcile)
	}
	// The pending file MUST survive a transient read so the next start retries.
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("pending file must be KEPT after a transient read error (#2149); stat err = %v", err)
	}
}

// TestSandboxReconcile_BailsWhenStopCtxCancelled pins R20260613-GO-003: if
// stopCtx is already cancelled when reconcileSandboxPending enters the loop,
// it must return immediately without calling StopSession on any remaining
// pending record.
func TestSandboxReconcile_BailsWhenStopCtxCancelled(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, _ := sandboxTestScheduler(t, runner, storePath)
	j := sandboxJob(t, s)

	// Write two pending files so the loop would call StopSession twice if it
	// did not respect the stopCtx cancellation.
	writePendingFixture(t, storePath, sandboxPending{
		JobID: j.ID, RunID: "aabbccddeeff0011",
		RuntimeSessionID: "run-aabbccddeeff0011-1234567890123456789",
		StartedAtMS:      time.Now().Add(-2 * time.Minute).UnixMilli(),
	})
	writePendingFixture(t, storePath, sandboxPending{
		JobID: j.ID, RunID: "1122334455667788",
		RuntimeSessionID: "run-1122334455667788-1234567890123456789",
		StartedAtMS:      time.Now().Add(-3 * time.Minute).UnixMilli(),
	})

	// Cancel stopCtx before the reconcile pass starts.
	s.Stop()

	s.reconcileSandboxPending()

	runner.mu.Lock()
	nStopped := len(runner.stopped)
	runner.mu.Unlock()
	if nStopped != 0 {
		t.Fatalf("StopSession called %d time(s) after stopCtx was cancelled; want 0", nStopped)
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
