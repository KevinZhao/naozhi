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
		state RunState
		in    int
		want  int
	}{
		{RunStateFailed, 0, 1},
		{RunStateTimedOut, 3, 4},
		{RunStateSucceeded, 4, 0},
		{RunStateSkipped, 4, 4},
		{RunStateCanceled, 4, 4},
	}
	for _, tc := range cases {
		if got := nextFailureStreak(tc.in, tc.state); got != tc.want {
			t.Errorf("nextFailureStreak(%d, %q) = %d, want %d", tc.in, tc.state, got, tc.want)
		}
	}
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
	if _, _, ok := s.recordTerminalResult(j.ID, "", "boom", "", ErrClassSendError, RunStateFailed, time.Now()); ok {
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
	rc.snap.platName = "feishu"

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

// TestAutoPause_SandboxClosesWithoutNoticeAnnouncePause covers the two
// sandbox terminal paths that send no per-run notice: a panicking replay and
// a restart-orphaned run. The close that pauses the job still announces it.
func TestAutoPause_SandboxClosesWithoutNoticeAnnouncePause(t *testing.T) {
	t.Parallel()
	const suffix = "；已连续失败 1 次，任务已自动暂停，修复后在控制台恢复"

	t.Run("replay panic", func(t *testing.T) {
		t.Parallel()
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
	})
	t.Run("orphan", func(t *testing.T) {
		t.Parallel()
		s, _, ns, j := pausingSandboxScheduler(t, &fakeSandboxRunner{})
		writePendingFixture(t, s.storePath, sandboxstore.Pending{
			JobID: j.ID, RunID: "abcabcabc0000110",
			RuntimeSessionID: "run-abcabcabc0000110-1234567890123456789",
			StartedAtMS:      time.Now().Add(-2 * time.Minute).UnixMilli(),
		})
		s.reconcileSandboxPending()
		if !s.jobForTest(t, j.ID).Paused {
			t.Fatal("the orphan's failure did not pause the job")
		}
		want := "[Cron push a PR] 云沙箱连接中断，任务状态未知，请检查执行历史 · run abcabcab" + suffix
		if got := ns.noticesAfter(s); len(got) != 1 || got[0] != want {
			t.Errorf("notices = %q, want [%q]", got, want)
		}
	})
}
