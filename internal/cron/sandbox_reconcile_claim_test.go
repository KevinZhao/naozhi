package cron

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cron/sandboxstore"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// The startup reconcile must only touch pending records that were on disk when
// Start ran. A sandbox run this process starts while the async pass is still
// pending writes its own record; reconciling that one would Stop a live microVM
// and overwrite the run's result. The pass is held until the live run's record
// exists so the interleaving is forced, not hoped for.
func TestStart_SandboxReconcileSkipsRunsStartedAfterStart(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "cron_jobs.json")
	runGo := make(chan struct{})
	runner := &fakeSandboxRunner{outcome: SandboxOutcome{State: SandboxStateSuccess, ResultText: "ok"}}
	probe := &probeRunner{inner: runner, onRun: func(SandboxJob) { <-runGo }}
	s, rec := sandboxTestScheduler(t, probe, storePath)
	passGo := make(chan struct{})
	s.startupPassHook = func(pass string) {
		if pass == "sandbox-pending-reconcile" {
			<-passGo
		}
	}
	releasePass := sync.OnceFunc(func() { close(passGo) })
	releaseRun := sync.OnceFunc(func() { close(runGo) })
	t.Cleanup(func() { releasePass(); releaseRun() })

	const orphanSID = "run-feedfacefeedface-1234567890123456789"
	orphanPath := writePendingFixture(t, storePath, sandboxstore.Pending{
		JobID: "abcdefabcdef0123", RunID: "feedfacefeedface",
		RuntimeSessionID: orphanSID,
		StartedAtMS:      time.Now().Add(-5 * time.Minute).UnixMilli(),
	})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	j := sandboxJob(t, s)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		s.executeOpt(j.ID, true)
	}()
	testhelper.Eventually(t, func() bool {
		entries, _ := s.sandboxState().ListPending()
		return len(entries) == 2
	}, 5*time.Second, "the live run never wrote its pending record")

	releasePass()
	s.gcWG.Wait()
	runner.mu.Lock()
	stopped := slices.Clone(runner.stopped)
	runner.mu.Unlock()
	if !slices.Equal(stopped, []string{orphanSID}) {
		t.Errorf("StopSession calls = %v, want only the previous process's orphan %q", stopped, orphanSID)
	}
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Errorf("the previous process's pending record survived the reconcile: %v", err)
	}

	releaseRun()
	<-runDone
	got, ok := s.GetJob(j.ID)
	if !ok {
		t.Fatal("job vanished")
	}
	if c := got.RunCounters; c.Total != 1 || c.Succeeded != 1 {
		t.Errorf("RunCounters = %+v, want the live run's own success", c)
	}
	if got.LastError != "" {
		t.Errorf("LastError = %q, want empty: the startup reconcile closed the live run", got.LastError)
	}
	for i := 0; i < rec.endedCount(); i++ {
		if ev := rec.endedAtCron(i); ev.JobID == j.ID && ev.State != RunStateSucceeded {
			t.Errorf("ended event for the live run: %s/%s, want succeeded", ev.State, ev.ErrorClass)
		}
	}
}
