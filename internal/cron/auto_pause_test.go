package cron

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cron/sandboxstore"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// streakRouter scripts how the next run ends: a spawn refusal (getErr), a
// failed send (sendErr, returned with sendRes), or a success when both are nil.
type streakRouter struct {
	mu      sync.Mutex
	getErr  error
	sendErr error
	sendRes SendResult
	panics  bool
}

func (r *streakRouter) set(getErr, sendErr error) {
	r.mu.Lock()
	r.getErr, r.sendErr = getErr, sendErr
	r.mu.Unlock()
}

func (r *streakRouter) RegisterCronStubWithChain(string, string, string, []string) {}
func (r *streakRouter) Reset(string)                                               {}
func (r *streakRouter) GetOrCreate(context.Context, string, AgentOpts) (Session, SessionStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.panics {
		panic("spawn exploded")
	}
	if r.getErr != nil {
		return nil, SessionExisting, r.getErr
	}
	return streakSession{err: r.sendErr, res: r.sendRes}, SessionExisting, nil
}

type streakSession struct {
	err error
	res SendResult
}

func (s streakSession) Send(context.Context, string) (SendResult, error) {
	if s.err != nil {
		return s.res, s.err
	}
	return SendResult{Text: "ok", SessionID: "sess-ok"}, nil
}
func (s streakSession) SessionID() string                     { return "sess-ok" }
func (s streakSession) InterruptViaControl() InterruptOutcome { return InterruptUnsupported }

var (
	errStreakSend = errors.New("broken pipe")
	errStreakCap  = fmt.Errorf("%w: max exempt sessions reached", ErrSessionCapacity)
)

// newAutoPauseScheduler builds a scheduler with one IM job registered through
// AddJob (so it owns a cron entry) and a recording notify sender. threshold
// is passed through SchedulerConfig unchanged: 0 exercises the default.
func newAutoPauseScheduler(t *testing.T, threshold int, platform string) (*Scheduler, *streakRouter, *recordingNotifySender, string) {
	t.Helper()
	r := &streakRouter{}
	s := NewScheduler(SchedulerConfig{
		MaxJobs:                5,
		StorePath:              filepath.Join(t.TempDir(), "cron_jobs.json"),
		AutoPauseAfterFailures: threshold,
	}, SchedulerDeps{Router: r})
	ns := &recordingNotifySender{}
	s.configMapsPtr.Store(&cronConfigMaps{notifySender: ns})
	j := NewJob("@every 10m", "ping", JobIMContext{Platform: platform, ChatID: "chat-1"})
	if platform == "dashboard" {
		j.NotifyPlatform, j.NotifyChatID = "feishu", "chat-1"
	}
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	return s, r, ns, j.ID
}

// runN drives n complete runs through executeOpt (TriggerNow path, no jitter).
// Each run's notice goes out on its own goroutine; waiting for it before the
// next run keeps the recorded notices in run order.
func runN(s *Scheduler, id string, n int) {
	for range n {
		s.executeOpt(id, true)
		s.triggerWG.Wait()
	}
}

// pausingNotice returns the last notice and fails unless it is the only one
// that announces the auto-pause.
func pausingNotice(t *testing.T, notices []string) string {
	t.Helper()
	var pauses []int
	for i, n := range notices {
		if strings.Contains(n, "自动暂停") {
			pauses = append(pauses, i)
		}
	}
	if len(pauses) != 1 || pauses[0] != len(notices)-1 {
		t.Fatalf("pause announced at notice indexes %v of %d, want only the last: %q", pauses, len(notices), notices)
	}
	return notices[len(notices)-1]
}

// persistedJob reloads job id from the store file the way Start does.
func persistedJob(t *testing.T, s *Scheduler, id string) *Job {
	t.Helper()
	jobs, err := loadJobs(s.storePath)
	if err != nil || jobs[id] == nil {
		t.Fatalf("loadJobs: %v (job present: %v)", err, jobs[id] != nil)
	}
	return jobs[id]
}

func TestNextFailureStreak(t *testing.T) {
	t.Parallel()
	cases := []struct {
		state  RunState
		class  ErrorClass
		cause  TurnCause
		in     int
		want   int
		orphan bool
	}{
		{RunStateFailed, ErrClassSendError, "", 0, 1, false},
		{RunStateFailed, ErrClassSessionError, "", 2, 3, false},
		{RunStateTimedOut, ErrClassDeadlineExceeded, "", 3, 4, false},
		{RunStateTimedOut, ErrClassSandboxTransport, "", 3, 4, false},
		// A live lost sandbox stream counts; only a restart orphan does not.
		{RunStateFailed, ErrClassSandboxTransport, "", 3, 4, false},
		{RunStateFailed, ErrClassSandboxTransport, "", 3, 3, true},
		{RunStateFailed, ErrClassTurnFailed, TurnCauseBackendOverloaded, 3, 3, false},
		{RunStateFailed, ErrClassTurnFailed, TurnCauseBackendRateLimited, 3, 3, false},
		{RunStateFailed, ErrClassTurnFailed, TurnCauseBackendUnreachable, 3, 3, false},
		{RunStateFailed, ErrClassTurnFailed, TurnCauseQuota, 3, 4, false},
		{RunStateFailed, ErrClassTurnFailed, TurnCauseBackendAuth, 3, 4, false},
		{RunStateFailed, ErrClassTurnFailed, TurnCauseMaxTurns, 3, 4, false},
		{RunStateFailed, ErrClassTurnFailed, TurnCauseContextTooLong, 3, 4, false},
		{RunStateFailed, ErrClassTurnFailed, TurnCauseUnknown, 3, 4, false},
		// A transient cause only exempts a failed turn, not another class.
		{RunStateFailed, ErrClassSendError, TurnCauseBackendOverloaded, 3, 4, false},
		{RunStateSucceeded, ErrClassNone, "", 4, 0, false},
		{RunStateSkipped, ErrClassSessionCapacity, "", 4, 4, false},
		{RunStateCanceled, ErrClassInterrupted, "", 4, 4, false},
	}
	for _, tc := range cases {
		out := runOutcome{state: tc.state, errClass: tc.class, turnCause: tc.cause, restartOrphan: tc.orphan}
		if got := nextFailureStreak(tc.in, out.streakEffect()); got != tc.want {
			t.Errorf("nextFailureStreak(%d, %q/%q/%q orphan=%v) = %d, want %d", tc.in, tc.state, tc.class, tc.cause, tc.orphan, got, tc.want)
		}
	}
}

// TestAutoPause_TransientBackendFailureDoesNotCount: a turn failed by a
// transient backend cause is recorded and notified as a failure but leaves
// the streak where it was; the next counted failure then pauses the job.
func TestAutoPause_TransientBackendFailureDoesNotCount(t *testing.T) {
	t.Parallel()
	for _, cause := range []TurnCause{TurnCauseBackendOverloaded, TurnCauseBackendRateLimited, TurnCauseBackendUnreachable} {
		t.Run(string(cause), func(t *testing.T) {
			t.Parallel()
			s, r, ns, id := newAutoPauseScheduler(t, 3, "feishu")
			r.set(nil, errStreakSend)
			runN(s, id, 2)

			r.set(nil, &TurnFailedError{Cause: cause})
			runN(s, id, 3)
			j := s.jobForTest(t, id)
			if j.Paused || j.ConsecutiveFailures != 2 {
				t.Fatalf("after transient failures: paused=%v streak=%d, want active with streak 2", j.Paused, j.ConsecutiveFailures)
			}
			if j.LastErrorClass != ErrClassTurnFailed || j.RunCounters.Failed != 5 {
				t.Fatalf("class=%q failed=%d, want turn_failed with 5 failed runs", j.LastErrorClass, j.RunCounters.Failed)
			}
			notices := ns.noticesAfter(s)
			last := notices[len(notices)-1]
			if !strings.Contains(last, turnFailedNotices[cause]) || strings.Contains(last, "自动暂停") {
				t.Errorf("transient failure notice = %q, want the cause's sentence and no pause", last)
			}

			r.set(nil, errStreakSend)
			runN(s, id, 1)
			if j := s.jobForTest(t, id); !j.Paused || j.ConsecutiveFailures != 3 {
				t.Errorf("third counted failure: paused=%v streak=%d, want paused at 3", j.Paused, j.ConsecutiveFailures)
			}
		})
	}
	// An active job whose streak already sits at the threshold (as after the
	// threshold was lowered) stays active, on the send and the spawn path.
	for name, spawn := range map[string]bool{"at threshold send": false, "at threshold spawn": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, r, _, id := newAutoPauseScheduler(t, 2, "feishu")
			s.editJobForTest(t, id, func(j *Job) { j.ConsecutiveFailures = 2 })
			err := &TurnFailedError{Cause: TurnCauseBackendOverloaded}
			if spawn {
				r.set(err, nil)
			} else {
				r.set(nil, err)
			}
			runN(s, id, 1)
			if j := s.jobForTest(t, id); j.Paused || j.ConsecutiveFailures != 2 || j.LastErrorClass != ErrClassTurnFailed {
				t.Errorf("paused=%v streak=%d class=%q, want an active turn_failed job at streak 2", j.Paused, j.ConsecutiveFailures, j.LastErrorClass)
			}
		})
	}
	t.Run("quota counts", func(t *testing.T) {
		t.Parallel()
		s, r, _, id := newAutoPauseScheduler(t, 1, "feishu")
		r.set(nil, &TurnFailedError{Cause: TurnCauseQuota})
		runN(s, id, 1)
		if j := s.jobForTest(t, id); !j.Paused {
			t.Errorf("a quota failure did not pause: streak=%d", j.ConsecutiveFailures)
		}
	})
}

// TestAutoPause_DefaultThresholdPausesOnFifthFailure drives real runs: four
// failures keep the job active with plain notices, the fifth pauses it —
// entry removed, state and reason persisted, metric bumped, and the notice
// says how to resume. A further trigger is refused.
func TestAutoPause_DefaultThresholdPausesOnFifthFailure(t *testing.T) {
	s, r, ns, id := newAutoPauseScheduler(t, 0, "feishu")
	r.set(nil, errStreakSend)
	before := metrics.CronAutoPausedTotal.Value()

	runN(s, id, 4)
	if j := s.jobForTest(t, id); j.Paused || j.ConsecutiveFailures != 4 {
		t.Fatalf("after 4 failures: paused=%v streak=%d, want active with streak 4", j.Paused, j.ConsecutiveFailures)
	}
	for _, n := range ns.noticesAfter(s) {
		if strings.Contains(n, "自动暂停") {
			t.Fatalf("notice before the threshold mentions a pause: %q", n)
		}
	}
	if len(s.cron.Entries()) != 1 {
		t.Fatalf("entries = %d before the pause, want 1", len(s.cron.Entries()))
	}

	runN(s, id, 1)
	j := s.jobForTest(t, id)
	if !j.Paused || j.PausedReason != PausedReasonAutoFailures || j.ConsecutiveFailures != 5 {
		t.Fatalf("after 5 failures: paused=%v reason=%q streak=%d", j.Paused, j.PausedReason, j.ConsecutiveFailures)
	}
	if n := len(s.cron.Entries()); n != 0 {
		t.Errorf("entries = %d after auto-pause, want 0", n)
	}
	if disk := persistedJob(t, s, id); !disk.Paused || disk.PausedReason != PausedReasonAutoFailures || disk.ConsecutiveFailures != 5 {
		t.Errorf("persisted = paused:%v reason:%q streak:%d", disk.Paused, disk.PausedReason, disk.ConsecutiveFailures)
	}
	if got := metrics.CronAutoPausedTotal.Value() - before; got != 1 {
		t.Errorf("CronAutoPausedTotal delta = %d, want 1", got)
	}
	last := pausingNotice(t, ns.noticesAfter(s))
	wantSuffix := "；已连续失败 5 次，任务已自动暂停，修复后发送 /cron resume " + id + " 恢复"
	if !strings.HasPrefix(last, "[Cron ping] 执行失败（CLI 发送错误） · run ") || !strings.HasSuffix(last, wantSuffix) {
		t.Errorf("pausing notice = %q, want the send-error body ending %q", last, wantSuffix)
	}
	if err := s.TriggerNow(id); !errors.Is(err, ErrJobPaused) {
		t.Errorf("TriggerNow after auto-pause = %v, want ErrJobPaused", err)
	}
}

// TestAutoPause_SuccessResetsAndSkipsDoNotCount: a success ends the streak;
// capacity skips neither extend nor end it.
func TestAutoPause_SuccessResetsAndSkipsDoNotCount(t *testing.T) {
	t.Parallel()
	s, r, ns, id := newAutoPauseScheduler(t, 3, "feishu")

	r.set(nil, errStreakSend)
	runN(s, id, 2)
	r.set(nil, nil)
	runN(s, id, 1)
	if got := s.jobForTest(t, id).ConsecutiveFailures; got != 0 {
		t.Fatalf("streak after a success = %d, want 0", got)
	}

	r.set(nil, errStreakSend)
	runN(s, id, 2)
	r.set(errStreakCap, nil)
	runN(s, id, 10)
	j := s.jobForTest(t, id)
	if j.Paused || j.ConsecutiveFailures != 2 {
		t.Fatalf("after capacity skips: paused=%v streak=%d, want active with streak 2", j.Paused, j.ConsecutiveFailures)
	}
	if j.LastErrorClass != ErrClassSessionCapacity {
		t.Fatalf("LastErrorClass = %q, the skips did not run", j.LastErrorClass)
	}

	r.set(errors.New("spawn boom"), nil)
	runN(s, id, 1)
	if j := s.jobForTest(t, id); !j.Paused || j.PausedReason != PausedReasonAutoFailures {
		t.Fatalf("third counted failure (spawn error) did not pause: paused=%v reason=%q", j.Paused, j.PausedReason)
	}
	if last := pausingNotice(t, ns.noticesAfter(s)); !strings.HasPrefix(last, "[Cron ping] 启动会话失败 · run ") ||
		!strings.HasSuffix(last, "；已连续失败 3 次，任务已自动暂停，修复后发送 /cron resume "+id+" 恢复") {
		t.Errorf("pausing spawn-error notice = %q", last)
	}
}

// TestAutoPause_ResumeClearsAndManualPauseHasNoReason: resume starts a fresh
// streak and drops the reason; a manual pause records none.
func TestAutoPause_ResumeClearsAndManualPauseHasNoReason(t *testing.T) {
	t.Parallel()
	s, r, _, id := newAutoPauseScheduler(t, 2, "feishu")
	r.set(nil, errStreakSend)
	runN(s, id, 2)

	if _, err := s.ResumeJobByID(id); err != nil {
		t.Fatalf("ResumeJobByID: %v", err)
	}
	j := s.jobForTest(t, id)
	if j.Paused || j.PausedReason != "" || j.ConsecutiveFailures != 0 {
		t.Fatalf("after resume: paused=%v reason=%q streak=%d", j.Paused, j.PausedReason, j.ConsecutiveFailures)
	}
	if n := len(s.cron.Entries()); n != 1 {
		t.Errorf("entries after resume = %d, want 1", n)
	}

	runN(s, id, 1)
	if _, err := s.PauseJobByID(id); err != nil {
		t.Fatalf("PauseJobByID: %v", err)
	}
	if j := s.jobForTest(t, id); j.PausedReason != "" {
		t.Errorf("manual pause reason = %q, want empty", j.PausedReason)
	}
}

// TestAutoPause_UpdateResetsStreakKeepsReason: an edit restarts the count;
// on an auto-paused job the reason stays until it is resumed.
func TestAutoPause_UpdateResetsStreakKeepsReason(t *testing.T) {
	t.Parallel()
	s, r, _, id := newAutoPauseScheduler(t, 2, "feishu")
	r.set(nil, errStreakSend)
	runN(s, id, 2)

	prompt := "ping v2"
	if _, err := s.UpdateJob(id, JobUpdate{Prompt: &prompt}); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	j := s.jobForTest(t, id)
	if j.ConsecutiveFailures != 0 || !j.Paused || j.PausedReason != PausedReasonAutoFailures {
		t.Errorf("after edit: streak=%d paused=%v reason=%q", j.ConsecutiveFailures, j.Paused, j.PausedReason)
	}
}

// TestAutoPause_NegativeThresholdDisables: a negative setting never pauses,
// and the streak is still tracked.
func TestAutoPause_NegativeThresholdDisables(t *testing.T) {
	t.Parallel()
	s, r, ns, id := newAutoPauseScheduler(t, -1, "feishu")
	r.set(nil, errStreakSend)
	runN(s, id, 8)
	if j := s.jobForTest(t, id); j.Paused || j.ConsecutiveFailures != 8 {
		t.Fatalf("paused=%v streak=%d, want active with streak 8", j.Paused, j.ConsecutiveFailures)
	}
	for _, n := range ns.noticesAfter(s) {
		if strings.Contains(n, "自动暂停") {
			t.Fatalf("disabled auto-pause still announced one: %q", n)
		}
	}
}

// TestAutoPause_DashboardJobPointsAtDashboard: a dashboard job cannot be
// resumed by /cron in the notify chat, so its notice points at the dashboard.
func TestAutoPause_DashboardJobPointsAtDashboard(t *testing.T) {
	t.Parallel()
	s, r, ns, id := newAutoPauseScheduler(t, 1, "dashboard")
	notify := true
	s.editJobForTest(t, id, func(j *Job) { j.Notify = &notify })
	r.set(nil, errStreakSend)
	runN(s, id, 1)
	notices := ns.noticesAfter(s)
	if len(notices) != 1 || !strings.HasSuffix(notices[0], "；已连续失败 1 次，任务已自动暂停，修复后在控制台恢复") {
		t.Fatalf("notices = %q", notices)
	}
}

// TestAutoPause_ResumeHintFollowsNotifyChat: /cron resume only finds a job
// from its own chat, so a pause notice delivered to another chat (a per-job
// override or notify_default) points back at the creating chat and the
// dashboard instead.
func TestAutoPause_ResumeHintFollowsNotifyChat(t *testing.T) {
	t.Parallel()
	crossChat := func(id string) string {
		return "；已连续失败 1 次，任务已自动暂停，修复后在创建该任务的会话发送 /cron resume " + id + "，或在控制台恢复"
	}
	cases := []struct {
		name string
		edit func(s *Scheduler, j *Job)
		want func(id string) string
	}{
		{"override to another chat", func(_ *Scheduler, j *Job) {
			j.NotifyPlatform, j.NotifyChatID = "feishu", "chat-2"
		}, crossChat},
		{"override to the source chat", func(_ *Scheduler, j *Job) {
			j.NotifyPlatform, j.NotifyChatID = "feishu", "chat-1"
		}, func(id string) string {
			return "；已连续失败 1 次，任务已自动暂停，修复后发送 /cron resume " + id + " 恢复"
		}},
		{"notify_default elsewhere", func(s *Scheduler, j *Job) {
			notify := true
			j.Notify = &notify
			s.notifyDefault = NotifyTarget{Platform: "slack", ChatID: "chat-1"}
		}, crossChat},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, r, ns, id := newAutoPauseScheduler(t, 1, "feishu")
			s.editJobForTest(t, id, func(j *Job) { tc.edit(s, j) })
			r.set(nil, errStreakSend)
			runN(s, id, 1)
			got := ns.noticesAfter(s)
			if len(got) != 1 || !strings.HasSuffix(got[0], tc.want(id)) {
				t.Errorf("notices = %q, want one ending %q", got, tc.want(id))
			}
		})
	}
}

func TestAutoPauseNoticeSuffix(t *testing.T) {
	t.Parallel()
	const head = "；已连续失败 3 次，任务已自动暂停，"
	src := NotifyTarget{Platform: "feishu", ChatID: "chat-1"}
	cases := []struct {
		name       string
		plat, chat string
		to         NotifyTarget
		paused     int
		want       string
	}{
		{"not paused", "feishu", "chat-1", src, 0, ""},
		{"source chat", "feishu", "chat-1", src, 3, head + "修复后发送 /cron resume j1 恢复"},
		{"other chat", "feishu", "chat-1", NotifyTarget{Platform: "feishu", ChatID: "chat-2"}, 3,
			head + "修复后在创建该任务的会话发送 /cron resume j1，或在控制台恢复"},
		{"other platform", "feishu", "chat-1", NotifyTarget{Platform: "slack", ChatID: "chat-1"}, 3,
			head + "修复后在创建该任务的会话发送 /cron resume j1，或在控制台恢复"},
		{"dashboard job", "dashboard", "dash", src, 3, head + "修复后在控制台恢复"},
		{"no source chat", "", "", src, 3, head + "修复后在控制台恢复"},
	}
	for _, tc := range cases {
		snap := jobSnapshot{jobID: "j1", platName: tc.plat, chatID: tc.chat}
		if got := autoPauseNoticeSuffix(snap, tc.to, tc.paused); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestAutoPause_PersistFailureLeavesJobActive: when the pause cannot be
// persisted the job keeps running and keeps its entry, and the notice does
// not claim a pause that did not happen.
func TestAutoPause_PersistFailureLeavesJobActive(t *testing.T) {
	t.Parallel()
	s, _, ns, id := newAutoPauseScheduler(t, 1, "feishu")
	s.editJobForTest(t, id, func(j *Job) { j.ConsecutiveFailures = 1 })
	withFailingMarshal(t, s)
	if got := s.autoPauseIfDue(id); got != 0 {
		t.Fatalf("autoPauseIfDue with a failing persist = %d, want 0", got)
	}
	j := s.jobForTest(t, id)
	if j.Paused || j.PausedReason != "" || j.ConsecutiveFailures != 1 {
		t.Errorf("after failed persist: paused=%v reason=%q streak=%d", j.Paused, j.PausedReason, j.ConsecutiveFailures)
	}
	if n := len(s.cron.Entries()); n != 1 {
		t.Errorf("entries = %d, want the entry kept", n)
	}
	if got := ns.noticesAfter(s); len(got) != 0 {
		t.Errorf("notices = %q, want none", got)
	}
}

// TestRecordResult_RevertRestoresStreak: a terminal result whose persist
// fails rolls the streak back with the rest of the result state.
func TestRecordResult_RevertRestoresStreak(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{StorePath: filepath.Join(t.TempDir(), "cron.json"), MaxJobs: 5, AllowNilRouter: true}, SchedulerDeps{})
	j := &Job{ID: "abcd00001111", Schedule: "@every 1h", ConsecutiveFailures: 2}
	s.putJobForTest(j)
	withFailingMarshal(t, s)
	if _, _, ok := s.recordTerminalResult(j.ID, runOutcome{errMsg: "boom", errClass: ErrClassSendError, state: RunStateFailed}, time.Now()); ok {
		t.Fatal("persist was expected to fail")
	}
	if got := s.jobForTest(t, j.ID).ConsecutiveFailures; got != 2 {
		t.Errorf("streak after reverted result = %d, want 2", got)
	}
}

// TestAutoPause_ResolveWorkspaceOutsideRootNotifiesOnPause: this path sends
// no per-run notice, but the run that pauses the job must announce it.
func TestAutoPause_ResolveWorkspaceOutsideRootNotifiesOnPause(t *testing.T) {
	t.Parallel()
	s, _, ns, id := newAutoPauseScheduler(t, 2, "feishu")
	s.allowedRoot = t.TempDir()
	outside := t.TempDir()
	rc := withNotify(newGetSessionArgs(t, s, &Job{ID: id}), "日报").runCtx
	rc.snap.workDir = outside
	rc.snap.platName, rc.snap.chatID = rc.notifyTo.Platform, rc.notifyTo.ChatID

	if _, abort := s.resolveCronWorkspace(rc); !abort {
		t.Fatal("outside-root work_dir must abort")
	}
	if got := ns.noticesAfter(s); len(got) != 0 {
		t.Fatalf("first failure sent %q, want no notice", got)
	}
	rc.finalizer = &runFinalizer{}
	rc.runID = "0123456789abcdef"
	if _, abort := s.resolveCronWorkspace(rc); !abort {
		t.Fatal("outside-root work_dir must abort")
	}
	want := "[Cron 日报] 工作目录超出允许根目录，本次执行已跳过 · run 01234567；已连续失败 2 次，任务已自动暂停，修复后发送 /cron resume " + id + " 恢复"
	if got := ns.noticesAfter(s); len(got) != 1 || got[0] != want {
		t.Errorf("notices = %q, want [%q]", got, want)
	}
}

// TestAutoPause_ConcurrentWithEntryWriters runs failing runs against
// resume/pause/update from other goroutines. It guards the lock order
// finishRun now takes (entryMu → s.tbl.mu, outside the gate): run under
// -race, a deadlock or race fails it.
func TestAutoPause_ConcurrentWithEntryWriters(t *testing.T) {
	t.Parallel()
	s, r, _, id := newAutoPauseScheduler(t, 1, "feishu")
	r.set(nil, errStreakSend)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 20 {
			s.executeOpt(id, true)
		}
	}()
	go func() {
		defer wg.Done()
		title := "t"
		for range 20 {
			_, _ = s.ResumeJobByID(id)
			_, _ = s.PauseJobByID(id)
			_, _ = s.UpdateJob(id, JobUpdate{Title: &title})
		}
	}()
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runs and entry writers deadlocked")
	}
	s.triggerWG.Wait()
	j := s.jobForTest(t, id)
	if j.PausedReason != "" && !j.Paused {
		t.Errorf("active job carries pause reason %q", j.PausedReason)
	}
}

// pauseObserver records, at each run_ended, whether the job already reads as
// paused there.
type pauseObserver struct {
	s           *Scheduler
	id          string
	mu          sync.Mutex
	pausedAtEnd []bool
}

func (o *pauseObserver) BroadcastRunStarted(runtelemetry.RunStartedEvent) {}
func (o *pauseObserver) BroadcastRunEnded(runtelemetry.RunEndedEvent) {
	_, paused := o.s.tbl.liveness(o.id)
	o.mu.Lock()
	o.pausedAtEnd = append(o.pausedAtEnd, paused)
	o.mu.Unlock()
}

// TestAutoPause_LandsBeforeRunEnded: the dashboard refetches the job list on
// run_ended, so the pausing run's run_ended must already see the job paused.
func TestAutoPause_LandsBeforeRunEnded(t *testing.T) {
	t.Parallel()
	obs := &pauseObserver{}
	r := &streakRouter{}
	s := NewScheduler(SchedulerConfig{
		MaxJobs: 5, StorePath: filepath.Join(t.TempDir(), "cron_jobs.json"), AutoPauseAfterFailures: 2,
	}, SchedulerDeps{Router: r, Telemetry: obs})
	j := NewJob("@every 10m", "ping", JobIMContext{Platform: "feishu", ChatID: "chat-1"})
	if err := s.AddJob(j); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	obs.s, obs.id = s, j.ID
	r.set(nil, errStreakSend)
	runN(s, j.ID, 2)
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.pausedAtEnd) != 2 || obs.pausedAtEnd[0] || !obs.pausedAtEnd[1] {
		t.Errorf("paused at each run_ended = %v, want [false true]", obs.pausedAtEnd)
	}
}

// TestAutoPause_ManualPauseClearsStaleReason: a reason left on an active job
// (a hand-edited store) does not survive a manual pause.
func TestAutoPause_ManualPauseClearsStaleReason(t *testing.T) {
	t.Parallel()
	s, _, _, id := newAutoPauseScheduler(t, 5, "feishu")
	s.editJobForTest(t, id, func(j *Job) { j.PausedReason = PausedReasonAutoFailures })
	if _, err := s.PauseJobByID(id); err != nil {
		t.Fatalf("PauseJobByID: %v", err)
	}
	if got := s.jobForTest(t, id).PausedReason; got != "" {
		t.Errorf("manual pause kept reason %q", got)
	}
}

// TestAutoPause_OtherNoticePathsCarrySuffix covers the terminal paths the
// executeOpt-driven tests do not reach: the fresh-mode work_dir preflight,
// the sandbox finish and the panic recovery.
func TestAutoPause_OtherNoticePathsCarrySuffix(t *testing.T) {
	t.Parallel()
	suffix := func(id string) string {
		return "；已连续失败 1 次，任务已自动暂停，修复后发送 /cron resume " + id + " 恢复"
	}

	t.Run("preflight work_dir", func(t *testing.T) {
		t.Parallel()
		s, _, ns, id := newAutoPauseScheduler(t, 1, "feishu")
		rc := withNotify(newGetSessionArgs(t, s, &Job{ID: id}), "日报").runCtx
		rc.snap.platName, rc.snap.chatID = rc.notifyTo.Platform, rc.notifyTo.ChatID
		rc.snap.fresh, rc.snap.workDir = true, t.TempDir()+"/missing"
		if _, ok := s.freshContextPreflightP0(preflightArgs{runCtx: rc}); ok {
			t.Fatal("unreachable work_dir must fail the preflight")
		}
		want := "[Cron 日报] 工作目录不可达，本次执行已跳过 · run 9f8e7d6c" + suffix(id)
		if got := ns.noticesAfter(s); len(got) != 1 || got[0] != want {
			t.Errorf("notices = %q, want [%q]", got, want)
		}
	})
	t.Run("sandbox", func(t *testing.T) {
		t.Parallel()
		s, _, ns, id := newAutoPauseScheduler(t, 1, "feishu")
		rc := withNotify(newGetSessionArgs(t, s, &Job{ID: id}), "日报").runCtx
		rc.snap.platName, rc.snap.chatID = rc.notifyTo.Platform, rc.notifyTo.ChatID
		s.finishSandboxRun(sandboxExecArgs{runCtx: rc}, RunStateFailed, ErrClassSandboxFailed, "", "boom", nil)
		want := "[Cron 日报] 云沙箱任务失败 · run 9f8e7d6c" + suffix(id)
		if got := ns.noticesAfter(s); len(got) != 1 || got[0] != want {
			t.Errorf("notices = %q, want [%q]", got, want)
		}
	})
	t.Run("panic", func(t *testing.T) {
		t.Parallel()
		s, r, ns, id := newAutoPauseScheduler(t, 1, "feishu")
		r.mu.Lock()
		r.panics = true
		r.mu.Unlock()
		runN(s, id, 1)
		if j := s.jobForTest(t, id); !j.Paused || j.LastErrorClass != ErrClassPanic {
			t.Fatalf("paused=%v class=%q, want a panic-paused job", j.Paused, j.LastErrorClass)
		}
		got := ns.noticesAfter(s)
		if len(got) != 1 || !strings.HasPrefix(got[0], "[Cron ping] 执行失败 · run ") || !strings.HasSuffix(got[0], suffix(id)) {
			t.Errorf("notices = %q, want one pausing panic notice", got)
		}
	})
}

// TestAutoPause_FailedFillKeepsReasonAndStreak: filling the prompt of an
// auto-paused job resumes it; when that cannot be persisted the job stays
// auto-paused with its reason and streak.
func TestAutoPause_FailedFillKeepsReasonAndStreak(t *testing.T) {
	t.Parallel()
	s, r, _, id := newAutoPauseScheduler(t, 2, "feishu")
	r.set(nil, errStreakSend)
	runN(s, id, 2)
	s.editJobForTest(t, id, func(j *Job) { j.Prompt = "" })
	withFailingMarshal(t, s)
	if err := s.SetJobPrompt(id, "ping v2"); err == nil {
		t.Fatal("SetJobPrompt with a failing persist succeeded")
	}
	j := s.jobForTest(t, id)
	if !j.Paused || j.PausedReason != PausedReasonAutoFailures || j.ConsecutiveFailures != 2 {
		t.Errorf("after failed fill: paused=%v reason=%q streak=%d, want auto-paused at 2", j.Paused, j.PausedReason, j.ConsecutiveFailures)
	}
}

// TestAutoPause_BelowThresholdSkipsEntryMu: a failure that cannot pause the
// job must not wait for entryMu, which a DeleteJob can hold for seconds.
func TestAutoPause_BelowThresholdSkipsEntryMu(t *testing.T) {
	t.Parallel()
	s, r, _, id := newAutoPauseScheduler(t, 5, "feishu")
	r.set(nil, errStreakSend)
	s.entryMu.Lock()
	defer s.entryMu.Unlock()
	done := make(chan struct{})
	go func() {
		s.executeOpt(id, true)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a below-threshold failure waited on entryMu")
	}
	if got := s.jobForTest(t, id).ConsecutiveFailures; got != 1 {
		t.Errorf("streak = %d, want 1", got)
	}
}

// pausingSandboxScheduler is a sandbox scheduler whose dashboard job pauses
// on its first failure and notifies a recording sender.
func pausingSandboxScheduler(t *testing.T, runner SandboxRunner) (*Scheduler, *recordingBroadcaster, *recordingNotifySender, *Job) {
	t.Helper()
	storePath := filepath.Join(t.TempDir(), "cron_jobs.json")
	s, rec := sandboxTestScheduler(t, runner, storePath)
	s.autoPauseAfter = 1
	ns := &recordingNotifySender{}
	s.configMapsPtr.Store(&cronConfigMaps{notifySender: ns})
	j := sideEffectsJob(t, s)
	s.editJobForTest(t, j.ID, func(j *Job) { j.NotifyPlatform, j.NotifyChatID = "feishu", "chat-1" })
	return s, rec, ns, j
}

// TestAutoPause_SandboxClosesWithoutNoticeAnnouncePause: a panicking replay
// sends no per-run notice; the close that pauses the job still announces it.
func TestAutoPause_SandboxClosesWithoutNoticeAnnouncePause(t *testing.T) {
	t.Parallel()
	const suffix = "；已连续失败 1 次，任务已自动暂停，修复后在控制台恢复"
	s, rec, ns, j := pausingSandboxScheduler(t, &panicReplayRunner{})
	s.sandboxState().WriteSnapshot(j.ID, "feedfacefeedface", "replay this prompt", "haiku", "img-v1", nil, slog.Default())
	if _, err := s.ReplaySandboxRun(j.ID, "feedfacefeedface"); err != nil {
		t.Fatalf("ReplaySandboxRun: %v", err)
	}
	waitEnded(t, rec)
	got := ns.noticesAfter(s)
	if len(got) != 1 || !strings.HasPrefix(got[0], "[Cron push a PR] 执行失败 · run ") || !strings.HasSuffix(got[0], suffix) {
		t.Errorf("notices = %q, want one pausing panic notice", got)
	}
}

// sandboxStreakScheduler is a pausingSandboxScheduler with threshold 2 whose
// job's streak already sits at seed.
func sandboxStreakScheduler(t *testing.T, seed int) (*Scheduler, *recordingNotifySender, *Job) {
	t.Helper()
	s, _, ns, j := pausingSandboxScheduler(t, &fakeSandboxRunner{})
	s.autoPauseAfter = 2
	s.editJobForTest(t, j.ID, func(j *Job) { j.ConsecutiveFailures = seed })
	return s, ns, j
}

// TestAutoPause_SandboxOrphanLeavesStreak: a run the restart reconciler closes
// as an orphan is a failed sandbox_transport run that neither extends nor
// resets the streak, even one already at the threshold, and sends no notice.
func TestAutoPause_SandboxOrphanLeavesStreak(t *testing.T) {
	t.Parallel()
	s, ns, j := sandboxStreakScheduler(t, 2)
	writePendingFixture(t, s.storePath, sandboxstore.Pending{
		JobID: j.ID, RunID: "abcabcabc0000110",
		RuntimeSessionID: "run-abcabcabc0000110-1234567890123456789",
		StartedAtMS:      time.Now().Add(-2 * time.Minute).UnixMilli(),
	})
	s.reconcileSandboxPending()
	got := s.jobForTest(t, j.ID)
	if got.Paused || got.ConsecutiveFailures != 2 {
		t.Errorf("paused=%v streak=%d, want active with streak 2", got.Paused, got.ConsecutiveFailures)
	}
	if got.LastErrorClass != ErrClassSandboxTransport || got.RunCounters.Failed != 1 {
		t.Errorf("class=%q failed=%d, want one failed sandbox_transport run", got.LastErrorClass, got.RunCounters.Failed)
	}
	if n := ns.noticesAfter(s); len(n) != 0 {
		t.Errorf("notices = %q, want none", n)
	}
}

// TestAutoPause_SandboxLiveTransportCounts: a live run whose sandbox stream
// was lost extends the streak like any other failure, so the run that reaches
// the threshold pauses the job and its one notice announces the pause.
func TestAutoPause_SandboxLiveTransportCounts(t *testing.T) {
	t.Parallel()
	s, ns, j := sandboxStreakScheduler(t, 1)
	rc := withNotify(newGetSessionArgs(t, s, j), "日报").runCtx
	s.finishSandboxRun(sandboxExecArgs{runCtx: rc}, RunStateFailed, ErrClassSandboxTransport, "", "stream lost", nil)
	got := s.jobForTest(t, j.ID)
	if !got.Paused || got.PausedReason != PausedReasonAutoFailures || got.ConsecutiveFailures != 2 {
		t.Errorf("paused=%v reason=%q streak=%d, want auto-paused at streak 2", got.Paused, got.PausedReason, got.ConsecutiveFailures)
	}
	want := "[Cron 日报] 云沙箱连接中断，任务状态未知，请检查执行历史 · run 9f8e7d6c" +
		"；已连续失败 2 次，任务已自动暂停，修复后在控制台恢复"
	if n := ns.noticesAfter(s); len(n) != 1 || n[0] != want {
		t.Errorf("notices = %q, want [%q]", n, want)
	}
}

// TestAutoPause_SandboxTransportBoundsSideEffectRepeats: a side-effecting job
// whose microVM loses the stream on every run is paused by the run that
// reaches the threshold, leaving one attention record per run and no more.
func TestAutoPause_SandboxTransportBoundsSideEffectRepeats(t *testing.T) {
	t.Parallel()
	const threshold = 3
	runner := &fakeSandboxRunner{outcome: SandboxOutcome{State: SandboxStateFailedTransport, ErrMsg: "stream reset"}}
	s, rec := sandboxTestScheduler(t, runner, filepath.Join(t.TempDir(), "cron_jobs.json"))
	s.autoPauseAfter = threshold
	j := sideEffectsJob(t, s)
	for i := 1; i <= threshold; i++ {
		s.executeOpt(j.ID, false)
		if n := rec.endedCount(); n != i {
			t.Fatalf("run %d: ended frames = %d, want %d", i, n, i)
		}
		if got := s.jobForTest(t, j.ID); got.Paused != (i == threshold) || got.ConsecutiveFailures != i {
			t.Fatalf("after run %d: paused=%v streak=%d, want paused=%v streak=%d", i, got.Paused, got.ConsecutiveFailures, i == threshold, i)
		}
	}
	if got := s.jobForTest(t, j.ID).PausedReason; got != PausedReasonAutoFailures {
		t.Errorf("PausedReason = %q, want %q", got, PausedReasonAutoFailures)
	}
	// The next tick finds the job paused and never reaches the microVM.
	s.executeOpt(j.ID, false)
	runner.mu.Lock()
	invoked := len(runner.gotJobs)
	runner.mu.Unlock()
	if n := s.sandboxState().AttentionCount(); invoked != threshold || n != threshold {
		t.Errorf("sandbox invocations = %d, attention records = %d, want %d each", invoked, n, threshold)
	}
}
