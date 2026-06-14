package cron

import (
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestReplay_PanicFinalizesBeforeRunEnded pins #2094 (R20260614-LB-replay): the
// dispatchReplay panic-recovery path must finalize the in-flight gate BEFORE it
// broadcasts the cron_run_ended frame, matching the finishRun
// finalize-before-emitRunEnded contract (R246-GO-3 / #689). Without the fix the
// goroutine's LIFO defer order ran emitRunEnded (recover defer, registered last)
// before finalizer.finalize() (registered earlier), so a concurrent dashboard
// list could observe a run-ended frame while CurrentRun(jobID) still reported
// the run as running.
//
// We drive the panic path with panicReplayRunner (panics inside RunJob, before
// finishSandboxRun) and capture CurrentRun visibility from inside the
// broadcaster callback — the exact concurrency window. Post-fix, CurrentRun
// must already be ok=false when the ended frame is broadcast.
func TestReplay_PanicFinalizesBeforeRunEnded(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")

	var sawRunning atomic.Bool // true if CurrentRun reported ok=true at emit time
	var jobID atomic.Pointer[string]

	var sched *Scheduler
	obs := observingBroadcaster{onEnded: func() {
		if p := jobID.Load(); p != nil {
			if _, ok := sched.CurrentRun(*p); ok {
				sawRunning.Store(true)
			}
		}
	}}
	sched = NewScheduler(
		SchedulerConfig{MaxJobs: 5, StorePath: storePath},
		SchedulerDeps{Router: panicRouter{t: t}, Telemetry: obs, Sandbox: &panicReplayRunner{}},
	)
	t.Cleanup(func() { sched.Stop() })

	j := sideEffectsJob(t, sched)
	id := j.ID
	jobID.Store(&id)
	origRunID := "feedfacefeedface"
	sched.writeSandboxSnapshot(j.ID, origRunID, "replay this prompt", "haiku", "img-v1", nil, slog.Default())

	if _, err := sched.ReplaySandboxRun(j.ID, origRunID); err != nil {
		t.Fatalf("ReplaySandboxRun: %v", err)
	}

	// The replay goroutine panics; the recover defer must still emit an ended
	// frame. Wait for it via the inflight gate clearing (finalize ran).
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := sched.CurrentRun(j.ID); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("inflight gate never cleared after panic-replay (finalize did not run)")
		}
		time.Sleep(2 * time.Millisecond)
	}

	if sawRunning.Load() {
		t.Fatal("#2094: CurrentRun still reported the run as running when cron_run_ended was broadcast — " +
			"emitRunEnded ran before finalize() in the panic-recovery path (finalize-before-broadcast contract violated)")
	}
}
