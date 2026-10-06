package cron

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/testhelper"
)

// newSchedulerRunStoreRefused builds a scheduler over storePath whose runs/ is
// a symlink, so the run store refuses it (#825) while every other state subtree,
// runinflight/ included, stays live.
func newSchedulerRunStoreRefused(t *testing.T, storePath string, router SessionRouter) *Scheduler {
	t.Helper()
	runs := filepath.Join(filepath.Dir(storePath), "runs")
	if _, err := os.Lstat(runs); errors.Is(err, os.ErrNotExist) {
		target := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, runs); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: storePath}, SchedulerDeps{Router: router})
	if s.runs.Enabled() {
		t.Fatal("run store enabled over a symlinked runs/; the fixture does not exercise the disabled mode")
	}
	if s.runInflightDir() == "" {
		t.Fatal("runinflight dir unresolved; markers would not be written either")
	}
	return s
}

// TestReconcileSettlesMarkersWithRunStoreDisabled: the writer only needs the
// store dir, so markers exist in this mode; the reader must consume them. The
// run ends through finishRun, so the job card says interrupted; history stays
// off because the refused store is disabled.
func TestReconcileSettlesMarkersWithRunStoreDisabled(t *testing.T) {
	t.Parallel()
	storePath := filepath.Join(t.TempDir(), "cron_jobs.json")
	s := newSchedulerRunStoreRefused(t, storePath, &fakeRouter{})

	jobID := mustGenerateID()
	s.putJobForTest(&Job{ID: jobID, Schedule: "@every 5m", Prompt: "do thing"})
	goneJob := mustGenerateID()
	for _, j := range []string{jobID, goneJob} {
		if path := s.writeRunInflightMarker(runInflightMarker{
			JobID: j, RunID: mustGenerateRunID(), Trigger: TriggerScheduled,
			StartedAtMS: time.Now().Add(-time.Minute).UnixMilli(),
		}, slog.Default()); path == "" {
			t.Fatal("marker write failed")
		}
	}

	s.reconcileRunInflight()

	if left := markerFiles(t, s); len(left) != 0 {
		t.Errorf("markers left after reconcile = %v; nothing would ever claim them", left)
	}
	j := s.jobForTest(t, jobID)
	if j.LastErrorClass != ErrClassInterrupted {
		t.Errorf("LastErrorClass = %q, want %q: the job card must show the restart", j.LastErrorClass, ErrClassInterrupted)
	}
	if j.LastRunAt.IsZero() || j.RunCounters.Canceled != 1 {
		t.Errorf("LastRunAt=%v Canceled=%d, want the interrupted run counted", j.LastRunAt, j.RunCounters.Canceled)
	}
}

// TestShutdownCancelMarkerAdoptedWithRunStoreDisabled is writer/reader parity:
// a shutdown-cancel keeps its marker on purpose (#2712), and the next process
// must claim it, adopting the live CLI instead of letting a tick send a
// second turn into it.
func TestShutdownCancelMarkerAdoptedWithRunStoreDisabled(t *testing.T) {
	t.Parallel()
	storePath := filepath.Join(t.TempDir(), "cron_jobs.json")
	s1 := newSchedulerRunStoreRefused(t, storePath, &fakeRouter{})
	jobID := mustGenerateID()
	job := &Job{ID: jobID, Schedule: "@every 5m", Prompt: "do thing"}
	s1.putJobForTest(job)
	runID := mustGenerateRunID()
	startedAt := time.Now().Add(-30 * time.Second)
	if path := s1.writeRunInflightMarker(runInflightMarker{
		JobID: jobID, RunID: runID, Trigger: TriggerScheduled, StartedAtMS: startedAt.UnixMilli(),
	}, slog.Default()); path == "" {
		t.Fatal("marker write failed")
	}
	s1.finishRun(runCtx{jobID: jobID, runID: runID, startedAt: startedAt, trigger: TriggerScheduled, lg: slog.Default()},
		runOutcome{state: RunStateCanceled, errClass: ErrClassCanceled, errMsg: "context canceled",
			skipPersist: true, keepInflightMarker: true})
	if left := markerFiles(t, s1); len(left) != 1 {
		t.Fatalf("markers after shutdown-cancel = %v, want the one kept for adoption", left)
	}

	key := "cron:" + jobID
	run := &fakeInFlightRun{
		outcome: AdoptedRunOutcome{Completed: true, Text: "the late answer", SubType: "success", SessionID: "sess-a1"},
		ready:   make(chan struct{}),
	}
	router := &adoptingRouter{
		verdicts: map[string]AdoptVerdict{key: AdoptLive},
		runs:     map[string]*fakeInFlightRun{key: run},
	}
	s2 := newSchedulerRunStoreRefused(t, storePath, router)
	s2.putJobForTest(job)

	settlement := s2.claimRunInflight()
	if settlement.adopted != 1 {
		t.Fatalf("adopted = %d, want 1: the kept marker was never claimed", settlement.adopted)
	}
	if _, won := s2.gateForTest().acquire(jobID); won {
		t.Fatal("run slot free during adoption; a fresh tick would double-run the job")
	}
	s2.settleRunInflight(settlement)
	close(run.ready)
	testhelper.Eventually(t, func() bool {
		return s2.jobForTest(t, jobID).LastResult == "the late answer"
	}, 5*time.Second, "the adopted run never reached the job card")
	s2.gcWG.Wait()
	if left := markerFiles(t, s2); len(left) != 0 {
		t.Errorf("markers left after the adoption settled = %v", left)
	}
}
