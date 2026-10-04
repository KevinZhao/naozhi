package cron

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// errTurnFailedRPC is what the wireup adapter returns for a turn the backend
// rejected over JSON-RPC: the sentinel plus run-history detail that must never
// reach the IM notice.
var errTurnFailedRPC = fmt.Errorf("%w (error): kiro rpc code -32001: upstream overloaded", ErrTurnFailed)

// TestClassifyExecError_TurnFailed: ErrTurnFailed is a failed run with its own
// class on both the send and spawn defaults; other errors keep the default.
func TestClassifyExecError_TurnFailed(t *testing.T) {
	t.Parallel()
	for _, def := range []ErrorClass{ErrClassSendError, ErrClassSessionError} {
		if st, cls := classifyExecError(errTurnFailedRPC, def); st != RunStateFailed || cls != ErrClassTurnFailed {
			t.Errorf("default %q: got (%q, %q), want (failed, turn_failed)", def, st, cls)
		}
	}
	if _, cls := classifyExecError(errors.New("broken pipe"), ErrClassSendError); cls != ErrClassSendError {
		t.Errorf("plain send error reclassified as %q", cls)
	}
}

// TestExecuteOpt_TurnFailedIsAFailedRun drives a whole run whose Send reports
// a failed turn: the record is failed/turn_failed with the detail in its error
// message, the failure streak advances, and the IM notice names the cause
// without the raw backend text or RPC code.
func TestExecuteOpt_TurnFailedIsAFailedRun(t *testing.T) {
	s, r, ns, id := newAutoPauseScheduler(t, 0, "feishu")
	r.set(nil, errTurnFailedRPC)

	runN(s, id, 1)

	j := s.jobForTest(t, id)
	if j.LastErrorClass != ErrClassTurnFailed {
		t.Errorf("LastErrorClass = %q, want turn_failed", j.LastErrorClass)
	}
	if j.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1: a failed turn counts toward auto-pause", j.ConsecutiveFailures)
	}
	if !strings.Contains(j.LastError, "turn failed") || !strings.Contains(j.LastError, "-32001") {
		t.Errorf("LastError = %q, want the turn-failure detail for run history", j.LastError)
	}
	if j.LastResult != "" {
		t.Errorf("LastResult = %q, want empty for a failed run", j.LastResult)
	}
	notices := ns.noticesAfter(s)
	if len(notices) != 1 {
		t.Fatalf("notices = %q, want exactly one", notices)
	}
	n := notices[0]
	if !strings.Contains(n, "执行失败（后端报告本轮出错），请检查执行历史") {
		t.Errorf("notice %q does not name the failed turn", n)
	}
	for _, raw := range []string{"-32001", "rpc", "overloaded", "turn failed"} {
		if strings.Contains(n, raw) {
			t.Errorf("notice %q leaks raw failure text %q", n, raw)
		}
	}
}

// TestAdoption_FailedTurnRecordsTurnFailed: an adopted turn whose late result
// reported a failure is recorded failed/turn_failed, not succeeded.
func TestAdoption_FailedTurnRecordsTurnFailed(t *testing.T) {
	t.Parallel()
	router := &adoptingRouter{verdicts: map[string]AdoptVerdict{}, runs: map[string]*fakeInFlightRun{}}
	s, jobID, runID, _ := seedMarkedRun(t, router, 0)
	key := "cron:" + jobID
	run := &fakeInFlightRun{
		outcome: AdoptedRunOutcome{
			Completed: true, SubType: "error_max_turns", SessionID: "sess-a1",
			TurnErr: fmt.Errorf("%w (error_max_turns)", ErrTurnFailed),
		},
		ready: make(chan struct{}),
	}
	router.verdicts[key] = AdoptLive
	router.runs[key] = run

	s.reconcileRunInflight()
	close(run.ready)

	got := waitRun(t, s, jobID, runID)
	if got.State != RunStateFailed || got.ErrorClass != ErrClassTurnFailed {
		t.Errorf("got (%s, %s), want (failed, turn_failed)", got.State, got.ErrorClass)
	}
	if !strings.HasPrefix(got.ErrorMsg, "send error: cron: turn failed") || !strings.Contains(got.ErrorMsg, "error_max_turns") {
		t.Errorf("ErrorMsg = %q, want the local path's shape with the failure detail", got.ErrorMsg)
	}
	s.gcWG.Wait()
}

// TestAdoption_FailedTurnThatPausesAnnouncesIt: an adopted run sends no
// per-run notice, but when its failed turn auto-pauses the job the pause is
// still announced to the job's notify target.
func TestAdoption_FailedTurnThatPausesAnnouncesIt(t *testing.T) {
	t.Parallel()
	router := &adoptingRouter{verdicts: map[string]AdoptVerdict{}, runs: map[string]*fakeInFlightRun{}}
	s, jobID, runID, _ := seedMarkedRun(t, router, 0)
	s.autoPauseAfter = 1
	ns := &recordingNotifySender{}
	s.configMapsPtr.Store(&cronConfigMaps{notifySender: ns})
	s.editJobForTest(t, jobID, func(j *Job) { j.NotifyPlatform, j.NotifyChatID = "feishu", "chat-1" })
	key := "cron:" + jobID
	run := &fakeInFlightRun{
		outcome: AdoptedRunOutcome{
			Completed: true, SubType: "error_max_turns",
			TurnErr: fmt.Errorf("%w (error_max_turns)", ErrTurnFailed),
		},
		ready: make(chan struct{}),
	}
	router.verdicts[key] = AdoptLive
	router.runs[key] = run

	s.reconcileRunInflight()
	close(run.ready)
	waitRun(t, s, jobID, runID)
	s.gcWG.Wait()

	if !s.jobForTest(t, jobID).Paused {
		t.Fatal("the adopted failure did not pause the job")
	}
	want := "执行失败（后端报告本轮出错），请检查执行历史 · run " + runID[:8] + "；已连续失败 1 次，任务已自动暂停"
	if got := ns.noticesAfter(s); len(got) != 1 || !strings.Contains(got[0], want) {
		t.Errorf("notices = %q, want one containing %q", got, want)
	}
}
