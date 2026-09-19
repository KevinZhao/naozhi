package cron

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestSkipPersistBroadcastErrorMsgIsRedacted: WS broadcast on a
// ctx-canceled (skipPersist=true) path must not leak absolute paths in
// ErrorMsg. The pre-fix code passed a.errMsg directly into RunEndedEvent
// which carried unredacted err.Error() output to all dashboard clients.
func TestSkipPersistBroadcastErrorMsgIsRedacted(t *testing.T) {
	t.Parallel()
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: &fakeRouter{}, Telemetry: rec})

	jobID := mustGenerateID()
	j := &Job{ID: jobID, Schedule: "@every 5m"}
	s.tblForTest().mu.Lock()
	s.tblForTest().jobs[jobID] = j
	s.tblForTest().mu.Unlock()

	rawErr := "session error: open /home/ops/private-secret/file.go: " + context.Canceled.Error()
	s.finishRun(finishArgs{
		job: j, runID: mustGenerateRunID(), startedAt: time.Now(),
		trigger: TriggerScheduled, state: RunStateCanceled,
		errClass: ErrClassCanceled, errMsg: rawErr, skipPersist: true,
	})

	if rec.endedCount() != 1 {
		t.Fatalf("want 1 ended event, got %d", rec.endedCount())
	}
	got := rec.endedAtCron(0)
	if strings.Contains(got.ErrorMsg, "/home/ops/private-secret/file.go") {
		t.Fatalf("path leaked to broadcast ErrorMsg: %q", got.ErrorMsg)
	}
	if !strings.Contains(got.ErrorMsg, "<path>") {
		t.Errorf("expected redaction sentinel in broadcast: %q", got.ErrorMsg)
	}
	// Sanity: error class still surfaces.
	if got.ErrorClass != ErrClassCanceled {
		t.Errorf("error_class lost in skipPersist path: %q", got.ErrorClass)
	}
}

// TestSuccessPathBroadcastUsesPersistedErrMsg: even on the success
// branch (errMsg empty), the broadcast must use persistedErrMsg, not
// a.errMsg. A regression here would re-introduce the leak.
func TestSuccessPathBroadcastUsesPersistedErrMsg(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: tmp + "/cron_jobs.json"}, SchedulerDeps{Router: &fakeRouter{}, Telemetry: rec})
	jobID := mustGenerateID()
	j := &Job{ID: jobID, Schedule: "@every 5m"}
	s.tblForTest().mu.Lock()
	s.tblForTest().jobs[jobID] = j
	s.tblForTest().mu.Unlock()

	rawErr := "send error: dial tcp 10.0.0.1:443: connect: connection refused"
	s.finishRun(finishArgs{
		job: j, runID: mustGenerateRunID(), startedAt: time.Now(),
		trigger: TriggerScheduled, state: RunStateFailed,
		errClass: ErrClassSendError, errMsg: rawErr,
	})
	if rec.endedCount() != 1 {
		t.Fatalf("want 1 ended event, got %d", rec.endedCount())
	}
	got := rec.endedAtCron(0)
	// R20260603-SEC-1/SEC-4: IP:port is now redacted; the broadcast must
	// not contain the raw address.
	if strings.Contains(got.ErrorMsg, "10.0.0.1") {
		t.Errorf("IP address leaked to broadcast ErrorMsg: %q", got.ErrorMsg)
	}
	if !strings.Contains(got.ErrorMsg, "[redacted-addr]") {
		t.Errorf("expected [redacted-addr] sentinel in broadcast ErrorMsg: %q", got.ErrorMsg)
	}
	// Consistency invariant: broadcast must equal on-disk Job.LastError.
	s.tblForTest().mu.RLock()
	defer s.tblForTest().mu.RUnlock()
	if got.ErrorMsg != j.LastError {
		t.Errorf("broadcast ErrorMsg %q diverges from Job.LastError %q", got.ErrorMsg, j.LastError)
	}
}

// TestPersistFailureSkipsCronRun: when recordResultP0WithSanitised
// reports !ok (Job marshal failed, fields rolled back), the CronRun history
// must NOT be appended — otherwise dashboard list and timeline diverge.
//
// We can't easily inject marshal failure in a unit test (it would require
// poisoning the json package), so we exercise the "Job concurrently
// deleted" branch which also sets ok=false.
func TestPersistFailureSkipsCronRun(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: tmp + "/cron_jobs.json"}, SchedulerDeps{Router: &fakeRouter{}})

	// Construct a job that's NOT registered in s.tbl.jobs — recordResultP0
	// will hit the "_, ok := s.tbl.jobs[j.ID]; !ok" early-return path and
	// return ok=false without touching disk.
	j := &Job{ID: mustGenerateID(), Schedule: "@every 5m"}
	// Note: deliberately not adding to s.tbl.jobs.

	runID := mustGenerateRunID()
	s.finishRun(finishArgs{
		job: j, runID: runID, startedAt: time.Now(),
		trigger: TriggerScheduled, state: RunStateSucceeded, result: "x",
	})

	if rows := s.ListRuns(j.ID, 10, time.Time{}); len(rows) != 0 {
		t.Errorf("CronRun must not be persisted when Job persist failed; got %d rows", len(rows))
	}
}
