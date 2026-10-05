package cron

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var transientT0 = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)

var errTransientOverloaded = &TurnFailedError{Cause: TurnCauseBackendOverloaded}

// transientScheduler is newAutoPauseScheduler on a fake clock at transientT0.
func transientScheduler(t *testing.T, threshold int) (*Scheduler, *streakRouter, *recordingNotifySender, string, *fakeClock) {
	t.Helper()
	s, r, ns, id := newAutoPauseScheduler(t, threshold, "feishu")
	clk := &fakeClock{now: transientT0}
	s.clock = clk
	return s, r, ns, id, clk
}

// runAt drives one run of job id that ends at t0 + d.
func runAt(s *Scheduler, clk *fakeClock, id string, d time.Duration) {
	clk.set(transientT0.Add(d))
	runN(s, id, 1)
}

func TestTransientBackendFailure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		out  runOutcome
		want bool
	}{
		{runOutcome{state: RunStateFailed, errClass: ErrClassTurnFailed, turnCause: TurnCauseBackendOverloaded}, true},
		{runOutcome{state: RunStateFailed, errClass: ErrClassTurnFailed, turnCause: TurnCauseBackendRateLimited}, true},
		{runOutcome{state: RunStateFailed, errClass: ErrClassTurnFailed, turnCause: TurnCauseBackendUnreachable}, true},
		{runOutcome{state: RunStateFailed, errClass: ErrClassTurnFailed, turnCause: TurnCauseQuota}, false},
		{runOutcome{state: RunStateFailed, errClass: ErrClassSendError, turnCause: TurnCauseBackendOverloaded}, false},
		{runOutcome{state: RunStateTimedOut, errClass: ErrClassTurnFailed, turnCause: TurnCauseBackendOverloaded}, false},
		{runOutcome{state: RunStateFailed, errClass: ErrClassTurnFailed, turnCause: TurnCauseBackendOverloaded, restartOrphan: true}, false},
		{runOutcome{state: RunStateFailed, errClass: ErrClassSandboxTransport, restartOrphan: true}, false},
	}
	for _, tc := range cases {
		if got := tc.out.transientBackendFailure(); got != tc.want {
			t.Errorf("transientBackendFailure(%+v) = %v, want %v", tc.out, got, tc.want)
		}
	}
}

// TestJobRecordStreaks: only a transient backend failure moves the transient
// count, its first one stamps the start, and a success clears both streaks.
func TestJobRecordStreaks(t *testing.T) {
	t.Parallel()
	later := transientT0.Add(time.Hour)
	start := failureStreaks{consecutive: 2, transient: 1, transientSince: transientT0}
	cases := []struct {
		name      string
		in        failureStreaks
		e         streakEffect
		transient bool
		want      failureStreaks
	}{
		{"first transient stamps", failureStreaks{consecutive: 2}, streakKeep, true, failureStreaks{consecutive: 2, transient: 1, transientSince: later}},
		{"later transient keeps start", start, streakKeep, true, failureStreaks{consecutive: 2, transient: 2, transientSince: transientT0}},
		{"counted failure", start, streakExtend, false, failureStreaks{consecutive: 3, transient: 1, transientSince: transientT0}},
		{"orphan, skip or cancel", start, streakKeep, false, start},
		{"success", start, streakReset, false, failureStreaks{}},
	}
	for _, tc := range cases {
		var j Job
		j.setStreaks(tc.in)
		j.recordStreaks(tc.e, tc.transient, later)
		if got := j.streaks(); got != tc.want {
			t.Errorf("%s: streaks = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestAutoPause_TransientFailuresPauseOnceTheySpanTheWindow: transient backend
// failures at the threshold count do not pause a job until they have gone on
// for transientAutoPauseWindow; then the failing run pauses it and its notice
// says so.
func TestAutoPause_TransientFailuresPauseOnceTheySpanTheWindow(t *testing.T) {
	t.Parallel()
	s, r, ns, id, clk := transientScheduler(t, 3)
	r.set(nil, errTransientOverloaded)
	for i := range 3 {
		runAt(s, clk, id, time.Duration(i)*20*time.Minute)
	}
	j := s.jobForTest(t, id)
	if j.Paused || j.TransientFailures != 3 || !j.TransientFailingSince.Equal(transientT0) || j.ConsecutiveFailures != 0 {
		t.Fatalf("3 failures in 40m: paused=%v transient=%d since=%v streak=%d, want active at 3 since t0 with streak 0",
			j.Paused, j.TransientFailures, j.TransientFailingSince, j.ConsecutiveFailures)
	}
	runAt(s, clk, id, transientAutoPauseWindow-time.Second)
	if j := s.jobForTest(t, id); j.Paused {
		t.Fatalf("paused a second before the window closed, transient=%d", j.TransientFailures)
	}

	runAt(s, clk, id, transientAutoPauseWindow)
	j = s.jobForTest(t, id)
	if !j.Paused || j.PausedReason != PausedReasonAutoFailures || j.TransientFailures != 5 {
		t.Fatalf("failure at the window: paused=%v reason=%q transient=%d, want auto-paused at 5", j.Paused, j.PausedReason, j.TransientFailures)
	}
	if disk := persistedJob(t, s, id); !disk.Paused || disk.TransientFailures != 5 || !disk.TransientFailingSince.Equal(transientT0) {
		t.Errorf("persisted = paused:%v transient:%d since:%v", disk.Paused, disk.TransientFailures, disk.TransientFailingSince)
	}
	last := pausingNotice(t, ns.noticesAfter(s))
	if !strings.Contains(last, turnFailedNotices[TurnCauseBackendOverloaded]) ||
		!strings.HasSuffix(last, "；已连续失败 5 次，任务已自动暂停，修复后发送 /cron resume "+id+" 恢复") {
		t.Errorf("pausing notice = %q, want the overloaded sentence and the pause suffix at 5", last)
	}
}

// TestAutoPause_TransientFailuresBelowThresholdStayActive: a long window alone
// does not pause; a counted failure in between moves neither the transient
// count nor the verdict, and the threshold-th transient failure pauses.
func TestAutoPause_TransientFailuresBelowThresholdStayActive(t *testing.T) {
	t.Parallel()
	s, r, _, id, clk := transientScheduler(t, 3)
	r.set(nil, errTransientOverloaded)
	runAt(s, clk, id, 0)
	runAt(s, clk, id, 7*time.Hour)
	r.set(nil, errStreakSend)
	runAt(s, clk, id, 7*time.Hour+time.Minute)
	j := s.jobForTest(t, id)
	if j.Paused || j.TransientFailures != 2 || j.ConsecutiveFailures != 1 {
		t.Fatalf("paused=%v transient=%d streak=%d, want active with 2 transient and streak 1", j.Paused, j.TransientFailures, j.ConsecutiveFailures)
	}
	r.set(nil, errTransientOverloaded)
	runAt(s, clk, id, 8*time.Hour)
	if j := s.jobForTest(t, id); !j.Paused || j.TransientFailures != 3 {
		t.Errorf("third transient failure: paused=%v transient=%d, want paused at 3", j.Paused, j.TransientFailures)
	}
}

// TestAutoPause_TransientCountClearedBySuccessResumeEdit: the transient count
// starts over where the failure streak does.
func TestAutoPause_TransientCountClearedBySuccessResumeEdit(t *testing.T) {
	t.Parallel()
	seeded := func(j *Job) { j.TransientFailures, j.TransientFailingSince = 4, transientT0 }
	cleared := func(t *testing.T, s *Scheduler, id, after string) {
		t.Helper()
		if j := s.jobForTest(t, id); j.TransientFailures != 0 || !j.TransientFailingSince.IsZero() {
			t.Errorf("after %s: transient=%d since=%v, want cleared", after, j.TransientFailures, j.TransientFailingSince)
		}
	}
	t.Run("success", func(t *testing.T) {
		t.Parallel()
		s, r, _, id, clk := transientScheduler(t, 3)
		r.set(nil, errTransientOverloaded)
		runAt(s, clk, id, 0)
		runAt(s, clk, id, time.Hour)
		r.set(nil, nil)
		runAt(s, clk, id, 2*time.Hour)
		cleared(t, s, id, "a success")
	})
	t.Run("resume", func(t *testing.T) {
		t.Parallel()
		s, _, _, id, _ := transientScheduler(t, 3)
		s.editJobForTest(t, id, seeded)
		if _, err := s.PauseJobByID(id); err != nil {
			t.Fatalf("PauseJobByID: %v", err)
		}
		if _, err := s.ResumeJobByID(id); err != nil {
			t.Fatalf("ResumeJobByID: %v", err)
		}
		cleared(t, s, id, "a resume")
	})
	t.Run("edit", func(t *testing.T) {
		t.Parallel()
		s, _, _, id, _ := transientScheduler(t, 3)
		s.editJobForTest(t, id, seeded)
		prompt := "ping v2"
		if _, err := s.UpdateJob(id, JobUpdate{Prompt: &prompt}); err != nil {
			t.Fatalf("UpdateJob: %v", err)
		}
		cleared(t, s, id, "an edit")
	})
	t.Run("failed resume keeps it", func(t *testing.T) {
		t.Parallel()
		s, _, _, id, _ := transientScheduler(t, 3)
		s.editJobForTest(t, id, seeded)
		if _, err := s.PauseJobByID(id); err != nil {
			t.Fatalf("PauseJobByID: %v", err)
		}
		withFailingMarshal(t, s)
		if _, err := s.ResumeJobByID(id); err == nil {
			t.Fatal("ResumeJobByID with a failing persist succeeded")
		}
		if j := s.jobForTest(t, id); !j.Paused || j.TransientFailures != 4 || !j.TransientFailingSince.Equal(transientT0) {
			t.Errorf("after failed resume: paused=%v transient=%d since=%v, want paused with 4 since t0", j.Paused, j.TransientFailures, j.TransientFailingSince)
		}
	})
	t.Run("failed fill keeps it", func(t *testing.T) {
		t.Parallel()
		s, _, _, id, _ := transientScheduler(t, 3)
		s.editJobForTest(t, id, seeded)
		if _, err := s.PauseJobByID(id); err != nil {
			t.Fatalf("PauseJobByID: %v", err)
		}
		s.editJobForTest(t, id, func(j *Job) { j.Prompt = "" })
		withFailingMarshal(t, s)
		if err := s.SetJobPrompt(id, "ping v2"); err == nil {
			t.Fatal("SetJobPrompt with a failing persist succeeded")
		}
		if j := s.jobForTest(t, id); j.TransientFailures != 4 || !j.TransientFailingSince.Equal(transientT0) {
			t.Errorf("after failed fill: transient=%d since=%v, want 4 since t0", j.TransientFailures, j.TransientFailingSince)
		}
	})
}

// TestAutoPause_NegativeThresholdIgnoresTransientWindow: disabling auto-pause
// disables the transient rule too.
func TestAutoPause_NegativeThresholdIgnoresTransientWindow(t *testing.T) {
	t.Parallel()
	s, r, ns, id, clk := transientScheduler(t, -1)
	r.set(nil, errTransientOverloaded)
	for i := range 8 {
		runAt(s, clk, id, time.Duration(i)*time.Hour)
	}
	if j := s.jobForTest(t, id); j.Paused || j.TransientFailures != 8 {
		t.Fatalf("paused=%v transient=%d, want active with 8", j.Paused, j.TransientFailures)
	}
	for _, n := range ns.noticesAfter(s) {
		if strings.Contains(n, "自动暂停") {
			t.Fatalf("disabled auto-pause still announced one: %q", n)
		}
	}
}

// TestRecordResult_RevertRestoresTransientCount: a transient result whose
// persist fails rolls the transient count and its start back too.
func TestRecordResult_RevertRestoresTransientCount(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{StorePath: filepath.Join(t.TempDir(), "cron.json"), MaxJobs: 5, AllowNilRouter: true}, SchedulerDeps{})
	j := &Job{ID: "abcd00002222", Schedule: "@every 1h", ConsecutiveFailures: 1, TransientFailures: 2, TransientFailingSince: transientT0}
	s.putJobForTest(j)
	withFailingMarshal(t, s)
	out := runOutcome{errMsg: "overloaded", errClass: ErrClassTurnFailed, turnCause: TurnCauseBackendOverloaded, state: RunStateFailed}
	if _, _, ok := s.recordTerminalResult(j.ID, out, transientT0.Add(time.Hour)); ok {
		t.Fatal("persist was expected to fail")
	}
	want := failureStreaks{consecutive: 1, transient: 2, transientSince: transientT0}
	if got := s.jobForTest(t, j.ID); got.streaks() != want {
		t.Errorf("streaks after reverted result = %+v, want %+v", got.streaks(), want)
	}
}
