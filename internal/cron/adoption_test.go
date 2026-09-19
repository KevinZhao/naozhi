package cron

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/testhelper"
)

// adoptingRouter is fakeRouter plus the InFlightAdopter capability, the way the
// production wireup adapter carries it. Verdicts are scripted per key.
type adoptingRouter struct {
	fakeRouter
	verdicts map[string]AdoptVerdict
	runs     map[string]*fakeInFlightRun
}

func (a *adoptingRouter) AdoptInFlight(key string) (InFlightRun, AdoptVerdict) {
	v := a.verdicts[key]
	if v == AdoptLive {
		return a.runs[key], AdoptLive
	}
	return nil, v
}

// fakeInFlightRun resolves when the test says so.
type fakeInFlightRun struct {
	outcome AdoptedRunOutcome
	err     error
	ready   chan struct{} // closed when the outcome may be returned
}

func (f *fakeInFlightRun) AwaitAdopted(ctx context.Context) (AdoptedRunOutcome, error) {
	select {
	case <-f.ready:
		return f.outcome, f.err
	case <-ctx.Done():
		return AdoptedRunOutcome{}, ctx.Err()
	}
}

// seedMarkedRun simulates process A: a registered job whose run wrote its
// marker and never finished.
func seedMarkedRun(t *testing.T, router SessionRouter, attempts int) (s *Scheduler, jobID, runID, storePath string) {
	t.Helper()
	storePath = filepath.Join(t.TempDir(), "cron_jobs.json")
	s = NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: storePath}, SchedulerDeps{Router: router})
	jobID = mustGenerateID()
	j := &Job{ID: jobID, Schedule: "@every 5m", Prompt: "do thing", WorkDir: "/tmp/wd"}
	s.tblForTest().mu.Lock()
	s.tblForTest().jobs[jobID] = j
	s.tblForTest().mu.Unlock()
	runID = mustGenerateRunID()
	if path := s.writeRunInflightMarker(runInflightMarker{
		JobID: jobID, RunID: runID, Trigger: TriggerScheduled,
		StartedAtMS: time.Now().Add(-90 * time.Second).UnixMilli(),
		Prompt:      "do thing", WorkDir: "/tmp/wd", Attempts: attempts,
	}, slog.Default()); path == "" {
		t.Fatal("marker write failed")
	}
	return s, jobID, runID, storePath
}

func waitRun(t *testing.T, s *Scheduler, jobID, runID string) CronRun {
	t.Helper()
	var got CronRun
	testhelper.Eventually(t, func() bool {
		r, err := s.Run(jobID, runID)
		if err != nil {
			return false
		}
		got = *r
		return true
	}, 5*time.Second, "run record never appeared")
	return got
}

// TestAdoption_LiveRunCompletesAcrossRestart is #2712's acceptance shape in
// unit form: the CLI survived the restart mid-turn, its late result is latched
// (PR A), and the reconciler's adoption turns it into a SUCCEEDED record whose
// duration spans the restart — where Phase 0 recorded interrupted.
func TestAdoption_LiveRunCompletesAcrossRestart(t *testing.T) {
	t.Parallel()
	router := &adoptingRouter{verdicts: map[string]AdoptVerdict{}, runs: map[string]*fakeInFlightRun{}}
	s, jobID, runID, _ := seedMarkedRun(t, router, 0)
	key := "cron:" + jobID
	run := &fakeInFlightRun{
		outcome: AdoptedRunOutcome{Completed: true, Text: "the late answer", SubType: "success", SessionID: "sess-a1"},
		ready:   make(chan struct{}),
	}
	router.verdicts[key] = AdoptLive
	router.runs[key] = run

	s.reconcileRunInflight()

	// While the adoption waits, the job's run slot must be held: a tick that
	// fires now must lose the CAS instead of double-running the job.
	if _, won := s.gateForTest().acquire(jobID); won {
		t.Fatal("run slot free during adoption; a fresh tick would double-run the job")
	}
	// ...and the marker must still exist (it is the only durable record).
	if _, err := s.Run(jobID, runID); err == nil {
		t.Fatal("run record exists before the adoption settled")
	}

	close(run.ready)
	got := waitRun(t, s, jobID, runID)
	if got.State != RunStateSucceeded {
		t.Errorf("State = %s, want succeeded", got.State)
	}
	if got.Result != "the late answer" {
		t.Errorf("Result = %q — the latched text did not reach the record", got.Result)
	}
	if got.SessionID != "sess-a1" {
		t.Errorf("SessionID = %q", got.SessionID)
	}
	if got.CostUSD != 0 {
		t.Errorf("CostUSD = %v, want 0 — an adopted run's cost is unknown, not guessed (#2750)", got.CostUSD)
	}
	if got.DurationMS < 80_000 {
		t.Errorf("DurationMS = %d, want the span across the restart (~90s)", got.DurationMS)
	}
	// Slot released, marker gone.
	testhelper.Eventually(t, func() bool {
		inf, won := s.gateForTest().acquire(jobID)
		if won {
			inf.running.Store(false)
		}
		return won
	}, 5*time.Second, "run slot never released after adoption settled")
	s.gcWG.Wait()
	// The marker must be deleted with the terminal record: a survivor would be
	// reconciled AGAIN next boot (Attempts=1 → interrupted) and append a second,
	// contradictory record for the same run.
	if _, err := os.Stat(filepath.Join(s.runInflightDir(), runID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("marker still on disk after the adoption settled (stat err=%v)", err)
	}
}

// TestAdoption_CLIDiesWithoutResult: the adopted turn ends with cli_exited —
// the record is Phase 0's interrupted, with the marker cleaned up.
func TestAdoption_CLIDiesWithoutResult(t *testing.T) {
	t.Parallel()
	router := &adoptingRouter{verdicts: map[string]AdoptVerdict{}, runs: map[string]*fakeInFlightRun{}}
	s, jobID, runID, _ := seedMarkedRun(t, router, 0)
	key := "cron:" + jobID
	run := &fakeInFlightRun{outcome: AdoptedRunOutcome{Completed: false}, ready: make(chan struct{})}
	router.verdicts[key] = AdoptLive
	router.runs[key] = run

	s.reconcileRunInflight()
	close(run.ready)

	got := waitRun(t, s, jobID, runID)
	if got.State != RunStateCanceled || got.ErrorClass != ErrClassInterrupted {
		t.Errorf("got (%s, %s), want (canceled, interrupted)", got.State, got.ErrorClass)
	}
	s.gcWG.Wait()
}

// TestAdoption_DriftShutdownRecordsConfigDrift (#2749): the shim survived but
// startup shut it down for argv drift — the operator's own config edit ended
// the run, and the record must say config_drift, not blame the restart.
func TestAdoption_DriftShutdownRecordsConfigDrift(t *testing.T) {
	t.Parallel()
	router := &adoptingRouter{verdicts: map[string]AdoptVerdict{}, runs: map[string]*fakeInFlightRun{}}
	s, jobID, runID, _ := seedMarkedRun(t, router, 0)
	router.verdicts["cron:"+jobID] = AdoptDriftShutdown

	s.reconcileRunInflight()

	got := waitRun(t, s, jobID, runID)
	if got.State != RunStateCanceled || got.ErrorClass != ErrClassConfigDrift {
		t.Errorf("got (%s, %s), want (canceled, config_drift)", got.State, got.ErrorClass)
	}
}

// TestAdoption_AttemptBoundStopsRetrying pins the crash-loop bound: a marker
// that already burned its adoption attempt is recorded interrupted, NOT
// adopted again — the worst case is one extra boot (#2751's class), never a
// loop with no exit.
func TestAdoption_AttemptBoundStopsRetrying(t *testing.T) {
	t.Parallel()
	router := &adoptingRouter{verdicts: map[string]AdoptVerdict{}, runs: map[string]*fakeInFlightRun{}}
	s, jobID, runID, _ := seedMarkedRun(t, router, maxAdoptAttempts)
	key := "cron:" + jobID
	// The router still says live — the bound must win anyway.
	router.verdicts[key] = AdoptLive
	router.runs[key] = &fakeInFlightRun{ready: make(chan struct{})}

	s.reconcileRunInflight()

	got := waitRun(t, s, jobID, runID)
	if got.State != RunStateCanceled || got.ErrorClass != ErrClassInterrupted {
		t.Errorf("got (%s, %s), want (canceled, interrupted) without a second adoption", got.State, got.ErrorClass)
	}
}

// TestAdoption_NoAdopterKeepsPhase0Behaviour: a router without the capability
// (every cron test fake, and any deployment predating the wireup change)
// degrades to exactly the old reconcile.
func TestAdoption_NoAdopterKeepsPhase0Behaviour(t *testing.T) {
	t.Parallel()
	s, jobID, runID, _ := seedMarkedRun(t, &fakeRouter{}, 0)
	s.reconcileRunInflight()
	got := waitRun(t, s, jobID, runID)
	if got.State != RunStateCanceled || got.ErrorClass != ErrClassInterrupted {
		t.Errorf("got (%s, %s), want the Phase 0 interrupted record", got.State, got.ErrorClass)
	}
}

// TestAdoption_ShutdownResolvesWait: Stop during an adoption must not hang —
// the wait is bound to stopCtx, and the record falls back to interrupted.
func TestAdoption_ShutdownResolvesWait(t *testing.T) {
	t.Parallel()
	router := &adoptingRouter{verdicts: map[string]AdoptVerdict{}, runs: map[string]*fakeInFlightRun{}}
	s, jobID, runID, _ := seedMarkedRun(t, router, 0)
	key := "cron:" + jobID
	router.verdicts[key] = AdoptLive
	router.runs[key] = &fakeInFlightRun{err: errors.New("never resolves"), ready: make(chan struct{})}

	s.reconcileRunInflight()
	s.Stop() // cancels stopCtx; the adoption goroutine must settle, gcWG must drain

	got := waitRun(t, s, jobID, runID)
	if got.ErrorClass != ErrClassInterrupted {
		t.Errorf("ErrorClass = %s, want interrupted after shutdown cut the wait", got.ErrorClass)
	}
}

// TestShutdownCancelKeepsMarker pins the discovery from the first real-machine
// end-to-end (#2712): a GRACEFUL shutdown — the way every actual upgrade
// restarts naozhi — cancels the in-flight Send via stopCtx, and that finish
// used to delete the restart marker. The CLI keeps running behind its shim,
// so the marker's claim is still true, and without it the adoption pass in
// the next process has nothing to adopt: the whole feature was reachable only
// after a hard kill. Shutdown-cancel must leave the marker; an operator
// interrupt must not.
func TestShutdownCancelKeepsMarker(t *testing.T) {
	t.Parallel()
	s, jobID, runID, _ := seedMarkedRun(t, &fakeRouter{}, 0)
	markerPath := filepath.Join(s.runInflightDir(), runID+".json")
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("seed marker missing: %v", err)
	}

	j, _ := func() (*Job, bool) {
		s.tblForTest().mu.RLock()
		defer s.tblForTest().mu.RUnlock()
		jj, ok := s.tblForTest().jobs[jobID]
		return jj, ok
	}()
	rc := runCtx{
		snap:      jobSnapshot{jobID: jobID, prompt: "p", workDir: "/tmp/wd"},
		startedAt: time.Now().Add(-30 * time.Second),
		runID:     runID, trigger: TriggerScheduled, job: j, lg: slog.Default(),
	}

	// Shutdown-cancel: marker survives.
	s.finishRunFor(rc, runOutcome{
		state: RunStateCanceled, errClass: ErrClassCanceled, errMsg: "context canceled",
		skipPersist: true, keepInflightMarker: true,
	})
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("shutdown-cancel deleted the marker; the next process cannot adopt this run: %v", err)
	}

	// Operator cancel (same skipPersist shape, keep flag off): marker cleared.
	s.finishRunFor(rc, runOutcome{
		state: RunStateCanceled, errClass: ErrClassCanceled, errMsg: "interrupted by operator",
		skipPersist: true,
	})
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("operator cancel left the marker (stat err=%v); next boot would fabricate an interrupted record for a run the operator ended", err)
	}
}
