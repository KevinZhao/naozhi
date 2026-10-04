package cron

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
// message and the session its result frame named, the failure streak advances,
// and the IM notice names the cause without the raw backend text or RPC code.
func TestExecuteOpt_TurnFailedIsAFailedRun(t *testing.T) {
	s, r, ns, id := newAutoPauseScheduler(t, 0, "feishu")
	rec := &recordingBroadcaster{}
	s.telemetry = rec
	r.set(nil, errTurnFailedRPC)
	r.sendRes = SendResult{SessionID: "sess-fail"}

	runN(s, id, 1)

	j := s.jobForTest(t, id)
	assertRunSession(t, s, rec, id, "sess-fail")
	if j.LastSessionID != "sess-fail" {
		t.Errorf("LastSessionID = %q, want the failed turn's session", j.LastSessionID)
	}
	if _, ok := s.KnownSessionIDs()["sess-fail"]; !ok {
		t.Error("KnownSessionIDs misses the failed turn's session; its JSONL leaks into recent sessions")
	}
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

// assertRunSession checks the one run of jobID carries sid both in runs/
// history and on its run-ended frame.
func assertRunSession(t *testing.T, s *Scheduler, rec *recordingBroadcaster, jobID, sid string) {
	t.Helper()
	runs := s.ListRuns(jobID, 10, time.Time{})
	if len(runs) != 1 || runs[0].SessionID != sid {
		t.Errorf("runs = %+v, want one run with SessionID %q", runs, sid)
	}
	if rec.endedCount() != 1 {
		t.Fatalf("ended events = %d, want 1", rec.endedCount())
	}
	if got := rec.endedAtCron(0).SessionID; got != sid {
		t.Errorf("RunEndedEvent.SessionID = %q, want %q", got, sid)
	}
}

// failingReapRouter is reapRouter whose session's Send fails with err,
// returning res alongside it. Each Reset also records the session id
// CurrentRun reports at that moment.
type failingReapRouter struct {
	reapRouter
	err      error
	res      SendResult
	s        *Scheduler
	jobID    string
	liveSIDs []string
}

func (r *failingReapRouter) Reset(key string) {
	r.reapRouter.Reset(key)
	view, _ := r.s.CurrentRun(r.jobID)
	r.mu.Lock()
	r.liveSIDs = append(r.liveSIDs, view.SessionID)
	r.mu.Unlock()
}

func (r *failingReapRouter) GetOrCreate(context.Context, string, AgentOpts) (Session, SessionStatus, error) {
	return streakSession{err: r.err, res: r.res}, SessionExisting, nil
}

// runFreshFailure runs one fresh-context job whose previous run left
// sess-prev and whose send fails as router scripts, and returns the chain of
// the last stub registered for it.
func runFreshFailure(t *testing.T, router *failingReapRouter) (*Scheduler, *recordingBroadcaster, string, []string) {
	t.Helper()
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, StorePath: filepath.Join(t.TempDir(), "cron_jobs.json")},
		SchedulerDeps{Router: router, Telemetry: rec})
	j := &Job{ID: mustGenerateID(), Schedule: "@every 5m", Prompt: "ping", FreshContext: true, LastSessionID: "sess-prev"}
	s.putJobForTest(j)
	router.s, router.jobID = s, j.ID

	s.executeOpt(j.ID, true)
	s.triggerWG.Wait()

	resets, regs := router.snapshot()
	if len(resets) < 2 {
		t.Errorf("resets = %v, want the preflight Reset and the failure reap", resets)
	}
	if len(regs) == 0 {
		t.Fatal("no stub registered")
	}
	return s, rec, j.ID, regs[len(regs)-1].chainIDs
}

// TestExecuteOpt_FreshTurnFailedChainsStubToFailedSession: after a fresh
// turn_failed the sidebar stub and LastSessionID point at the failed turn's
// JSONL, not the previous run's, and CurrentRun already names it while the
// failure is being finished.
func TestExecuteOpt_FreshTurnFailedChainsStubToFailedSession(t *testing.T) {
	t.Parallel()
	router := &failingReapRouter{err: errTurnFailedRPC, res: SendResult{SessionID: "sess-fail"}}
	s, rec, id, chain := runFreshFailure(t, router)

	if len(chain) != 1 || chain[0] != "sess-fail" {
		t.Errorf("stub chain = %v, want [sess-fail]", chain)
	}
	if live := router.liveSIDs; len(live) == 0 || live[len(live)-1] != "sess-fail" {
		t.Errorf("CurrentRun session at each Reset = %q, want sess-fail at the failure reap", live)
	}
	assertRunSession(t, s, rec, id, "sess-fail")
	if got := s.jobForTest(t, id).LastSessionID; got != "sess-fail" {
		t.Errorf("LastSessionID = %q, want sess-fail", got)
	}
	if _, ok := s.KnownSessionIDs()["sess-fail"]; !ok {
		t.Error("KnownSessionIDs misses the failed turn's session")
	}
}

// TestExecuteOpt_SendErrorWithoutResultKeepsSessionEmpty: a send error with
// no result frame records no session, keeps LastSessionID, and the fresh stub
// falls back to the previous run's chain. The session's own SessionID() is
// not borrowed.
func TestExecuteOpt_SendErrorWithoutResultKeepsSessionEmpty(t *testing.T) {
	t.Parallel()
	router := &failingReapRouter{err: errStreakSend}
	s, rec, id, chain := runFreshFailure(t, router)

	if len(chain) != 1 || chain[0] != "sess-prev" {
		t.Errorf("stub chain = %v, want [sess-prev]", chain)
	}
	assertRunSession(t, s, rec, id, "")
	if got := s.jobForTest(t, id).LastSessionID; got != "sess-prev" {
		t.Errorf("LastSessionID = %q, want sess-prev kept", got)
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
