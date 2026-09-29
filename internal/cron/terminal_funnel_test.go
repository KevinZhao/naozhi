package cron

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// A local run that panics after its started event is still closed: one
// run_ended (failed, panic), the inflight marker removed so the next boot does
// not read it as an interrupted run, the gauge back at its base and the ended
// counter moved once (#2897 C1). Not parallel: the counters are process-wide.
func TestLocalRunPanic_ClosesTheRun(t *testing.T) {
	endedBase := metrics.CronRunEndedTotal.Value()
	inflightBase := metrics.CronRunInflight.Value()
	rec := &recordingBroadcaster{}
	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: filepath.Join(dir, "cron_jobs.json")},
		SchedulerDeps{Router: &panickingRouter{}, Telemetry: rec})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	job := &Job{Schedule: "@hourly", Prompt: "panic-bait", Platform: "p", ChatID: "c"}
	if err := s.AddJob(job); err != nil {
		t.Fatal(err)
	}

	s.executeIfNotDeletedOrPaused(job.ID)

	if rec.startedCount() != 1 || rec.endedCount() != 1 {
		t.Fatalf("started=%d ended=%d, want one of each", rec.startedCount(), rec.endedCount())
	}
	ev := rec.endedAtCron(0)
	if ev.State != RunStateFailed || ev.ErrorClass != ErrClassPanic {
		t.Errorf("ended = %s/%s, want failed/panic", ev.State, ev.ErrorClass)
	}
	if d := metrics.CronRunEndedTotal.Value() - endedBase; d != 1 {
		t.Errorf("CronRunEndedTotal moved %d, want 1", d)
	}
	if metrics.CronRunInflight.Value() != inflightBase {
		t.Errorf("inflight gauge = %d, want %d", metrics.CronRunInflight.Value(), inflightBase)
	}
	entries, _ := os.ReadDir(s.runInflightDir())
	if len(entries) != 0 {
		t.Errorf("%d inflight marker(s) left; the next boot would mark the run interrupted", len(entries))
	}
	testhelper.Eventually(t, func() bool { return len(s.ListRuns(job.ID, 10, time.Time{})) == 1 },
		time.Second, "the panicked run has no history record")
	if _, running := s.CurrentRun(job.ID); running {
		t.Error("the job still shows a run in flight")
	}
}

// finishRun runs once per run: a second call for the same run is dropped, so
// no path can double-count or double-broadcast a run that another path already
// closed.
func TestFinishRun_OncePerRun(t *testing.T) {
	endedBase := metrics.CronRunEndedTotal.Value()
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: &fakeRouter{}, Telemetry: rec})
	job := &Job{ID: mustGenerateID(), Schedule: "@hourly", Prompt: "p"}
	rc := runCtx{job: job, runID: mustGenerateRunID(), startedAt: time.Now(), finalizer: &runFinalizer{}, term: &runTerm{}}
	s.finishRun(rc, runOutcome{state: RunStateSucceeded, skipPersist: true})
	s.finishRun(rc, runOutcome{state: RunStateFailed, errClass: ErrClassPanic, skipPersist: true})
	if rec.endedCount() != 1 || rec.endedAtCron(0).State != RunStateSucceeded {
		t.Errorf("ended events = %d (first %v), want the first finish only", rec.endedCount(), rec.endedAtCron(0).State)
	}
	if d := metrics.CronRunEndedTotal.Value() - endedBase; d != 1 {
		t.Errorf("CronRunEndedTotal moved %d, want 1", d)
	}
}

// A panic inside finishRun's persistence half still ends the run for
// subscribers (the broadcast half is deferred), and the scaffold's recover
// does not end it a second time.
func TestLocalRunPanicInsideFinish_EndsOnce(t *testing.T) {
	endedBase := metrics.CronRunEndedTotal.Value()
	rec := &recordingBroadcaster{}
	dir := t.TempDir()
	// The session lookup fails, so the run ends through finishRun normally and
	// the panic lands in finishRun's persistence half.
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: filepath.Join(dir, "cron_jobs.json")},
		SchedulerDeps{Router: &fakeRouter{getErr: errors.New("no session")}, Telemetry: rec})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	s.finishRunPreAppendHook = func(string) { panic("persist half") }
	job := &Job{Schedule: "@hourly", Prompt: "p", Platform: "p", ChatID: "c"}
	if err := s.AddJob(job); err != nil {
		t.Fatal(err)
	}

	s.executeIfNotDeletedOrPaused(job.ID)

	if rec.endedCount() != 1 {
		t.Fatalf("ended = %d, want exactly one", rec.endedCount())
	}
	if ev := rec.endedAtCron(0); ev.ErrorClass != ErrClassSessionError {
		t.Errorf("ended error class = %q, want the run's own %q (the recover must not re-close it)", ev.ErrorClass, ErrClassSessionError)
	}
	if d := metrics.CronRunEndedTotal.Value() - endedBase; d != 1 {
		t.Errorf("CronRunEndedTotal moved %d, want 1", d)
	}
	if _, running := s.CurrentRun(job.ID); running {
		t.Error("the job still shows a run in flight")
	}
}

// A run that panics before its persistence half settled still broadcasts a
// sanitised message, never the raw one.
func TestEndedErrMsg_NeverRaw(t *testing.T) {
	t.Parallel()
	raw := "open /home/op/secret/workspace/x: token=sk-ant-api03-" + strings.Repeat("A", 40) + "\x1b[31m"
	got := endedErrMsg(false, "", raw)
	if got == raw || got != sanitiseRunErrMsg(raw) {
		t.Errorf("unsettled message = %q, want sanitiseRunErrMsg(raw)", got)
	}
	if got := endedErrMsg(true, "persisted", raw); got != "persisted" {
		t.Errorf("settled message = %q, want the persisted copy", got)
	}
}
