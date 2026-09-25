package cron

import (
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/testhelper"
)

// The tests below pin #2799: a run the previous process started ends through
// finishRun, so everything a local finish updates is updated for it too. Before,
// both restart paths appended a CronRun directly, and none of the adoption
// tests looked at anything but that record — so the Job card kept showing the
// run from BEFORE the restart, counters skipped every cross-restart run, and no
// run_ended frame was sent, all while the suite stayed green.

// TestAdoption_SettleUpdatesJobStateAndBroadcasts: an adopted success is the
// job's latest run everywhere a local success would be.
func TestAdoption_SettleUpdatesJobStateAndBroadcasts(t *testing.T) {
	t.Parallel()
	router := &adoptingRouter{verdicts: map[string]AdoptVerdict{}, runs: map[string]*fakeInFlightRun{}}
	s, jobID, runID, _ := seedMarkedRun(t, router, 0)
	rb := &recordingBroadcaster{}
	s.SetTelemetry(rb)
	key := "cron:" + jobID
	run := &fakeInFlightRun{
		outcome: AdoptedRunOutcome{Completed: true, Text: "the late answer", SubType: "success", SessionID: "sess-a1"},
		ready:   make(chan struct{}),
	}
	router.verdicts[key] = AdoptLive
	router.runs[key] = run
	before := time.Now()

	s.reconcileRunInflight()
	close(run.ready)
	waitRun(t, s, jobID, runID)
	s.gcWG.Wait()

	j, ok := s.GetJob(jobID)
	if !ok {
		t.Fatal("job vanished")
	}
	if j.LastRunAt.Before(before) {
		t.Errorf("LastRunAt = %v, want the adopted run's end (after %v) — the card still shows the pre-restart run", j.LastRunAt, before)
	}
	if j.LastResult != "the late answer" {
		t.Errorf("LastResult = %q, want the adopted run's text", j.LastResult)
	}
	if j.LastSessionID != "sess-a1" {
		t.Errorf("LastSessionID = %q, want sess-a1", j.LastSessionID)
	}
	if j.LastErrorClass != "" {
		t.Errorf("LastErrorClass = %q, want empty after a success", j.LastErrorClass)
	}
	if j.RunCounters.Total != 1 || j.RunCounters.Succeeded != 1 {
		t.Errorf("RunCounters = %+v, want total=1 succeeded=1 — the cross-restart run went uncounted", j.RunCounters)
	}
	testhelper.Eventually(t, func() bool { return rb.endedCount() == 1 }, 5*time.Second, "no run_ended frame for the adopted run")
	rb.mu.Lock()
	ev := rb.ended[0]
	rb.mu.Unlock()
	if ev.RunID != runID || ev.State != RunStateSucceeded {
		t.Errorf("run_ended = %+v, want run %s succeeded", ev, runID)
	}
}

// TestInterruptedReconcile_UpdatesJobState: a run that could not be adopted
// ends interrupted, and the job card says so — the interrupted label #2711
// step 1 added was unreachable on the card while this path skipped
// recordTerminalResult.
func TestInterruptedReconcile_UpdatesJobState(t *testing.T) {
	t.Parallel()
	s, jobID, runID, _ := seedMarkedRun(t, &fakeRouter{}, 0)
	rb := &recordingBroadcaster{}
	s.SetTelemetry(rb)

	s.reconcileRunInflight()

	rec := waitRun(t, s, jobID, runID)
	if rec.State != RunStateCanceled || rec.ErrorClass != ErrClassInterrupted {
		t.Fatalf("record = %s/%s, want canceled/interrupted", rec.State, rec.ErrorClass)
	}
	j, ok := s.GetJob(jobID)
	if !ok {
		t.Fatal("job vanished")
	}
	if j.LastErrorClass != ErrClassInterrupted {
		t.Errorf("LastErrorClass = %q, want %q", j.LastErrorClass, ErrClassInterrupted)
	}
	if j.RunCounters.Total != 1 || j.RunCounters.Canceled != 1 {
		t.Errorf("RunCounters = %+v, want total=1 canceled=1", j.RunCounters)
	}
	if rb.endedCount() != 1 {
		t.Errorf("run_ended frames = %d, want 1", rb.endedCount())
	}
}

// TestAdoption_ResultIsSanitised: the adopted CLI's text goes through the same
// redaction every local result does. The direct CronRun write stored it raw,
// so a secret the CLI echoed landed in runs/ and the history API verbatim.
func TestAdoption_ResultIsSanitised(t *testing.T) {
	t.Parallel()
	router := &adoptingRouter{verdicts: map[string]AdoptVerdict{}, runs: map[string]*fakeInFlightRun{}}
	s, jobID, runID, _ := seedMarkedRun(t, router, 0)
	key := "cron:" + jobID
	run := &fakeInFlightRun{
		outcome: AdoptedRunOutcome{Completed: true, Text: "claude said: sk-ant-api03-abcdef0123456789 is your key", SubType: "success"},
		ready:   make(chan struct{}),
	}
	router.verdicts[key] = AdoptLive
	router.runs[key] = run

	s.reconcileRunInflight()
	close(run.ready)
	rec := waitRun(t, s, jobID, runID)
	s.gcWG.Wait()

	if strings.Contains(rec.Result, "sk-ant-") {
		t.Errorf("history record carries the secret verbatim: %q", rec.Result)
	}
	if j, _ := s.GetJob(jobID); strings.Contains(j.LastResult, "sk-ant-") {
		t.Errorf("Job.LastResult carries the secret verbatim: %q", j.LastResult)
	}
}
