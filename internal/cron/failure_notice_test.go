package cron

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/costledger"
)

// TestFailureNoticeBody pins the IM body per failure cause: the cause is named,
// timed-out bodies carry the budget, the run id is cut to the 8-char short form
// the dashboard history shows, and the old "请稍后重试" advice is gone.
func TestFailureNoticeBody(t *testing.T) {
	t.Parallel()
	const runID = "1a2b3c4d5e6f7a8b"
	cases := []struct {
		name    string
		class   ErrorClass
		state   RunState
		timeout time.Duration
		want    string
	}{
		{"send deadline", ErrClassDeadlineExceeded, RunStateTimedOut, 5 * time.Minute, "执行超时（超过 5m） · run 1a2b3c4d"},
		{"timed out state alone", ErrClassSendError, RunStateTimedOut, 90 * time.Second, "执行超时（超过 1m30s） · run 1a2b3c4d"},
		{"send error", ErrClassSendError, RunStateFailed, 5 * time.Minute, "执行失败（CLI 发送错误） · run 1a2b3c4d"},
		{"session error", ErrClassSessionError, RunStateFailed, 5 * time.Minute, "启动会话失败 · run 1a2b3c4d"},
		{"session capacity", ErrClassSessionCapacity, RunStateSkipped, 5 * time.Minute, "定时任务会话数已达上限，本次已跳过；可错开执行时间或改为每次重置上下文 · run 1a2b3c4d"},
		{"workdir unreachable", ErrClassWorkDirUnreachable, RunStateFailed, 5 * time.Minute, "工作目录不可达，本次执行已跳过 · run 1a2b3c4d"},
		{"workdir outside root", ErrClassWorkDirOutsideRoot, RunStateFailed, 5 * time.Minute, "工作目录超出允许根目录，本次执行已跳过 · run 1a2b3c4d"},
		{"sandbox failed", ErrClassSandboxFailed, RunStateFailed, time.Hour, "云沙箱任务失败 · run 1a2b3c4d"},
		{"sandbox unavailable", ErrClassSandboxUnavailable, RunStateFailed, time.Hour, "云沙箱未配置，任务无法执行 · run 1a2b3c4d"},
		{"sandbox transport", ErrClassSandboxTransport, RunStateFailed, time.Hour, "云沙箱连接中断，任务状态未知，请检查执行历史 · run 1a2b3c4d"},
		{"sandbox transport deadline", ErrClassSandboxTransport, RunStateTimedOut, time.Hour, "云沙箱运行超时（超过 1h），任务状态未知，请检查执行历史 · run 1a2b3c4d"},
		{"unclassified", ErrClassPanic, RunStateFailed, 5 * time.Minute, "执行失败 · run 1a2b3c4d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := failureNoticeBody(tc.class, tc.state, runID, tc.timeout)
			if got != tc.want {
				t.Errorf("failureNoticeBody(%q, %q) = %q, want %q", tc.class, tc.state, got, tc.want)
			}
			if strings.Contains(got, "请稍后重试") {
				t.Errorf("body %q still carries the generic retry advice", got)
			}
		})
	}
}

func TestFailureNoticeBody_ShortAndMissingRunID(t *testing.T) {
	t.Parallel()
	if got := failureNoticeBody(ErrClassSessionError, RunStateFailed, "abc", time.Minute); got != "启动会话失败 · run abc" {
		t.Errorf("short run id: got %q", got)
	}
	if got := failureNoticeBody(ErrClassSessionError, RunStateFailed, "", time.Minute); got != "启动会话失败" {
		t.Errorf("empty run id must drop the run suffix: got %q", got)
	}
}

// recordingNotifySender captures every IM text cron delivers.
type recordingNotifySender struct {
	mu    sync.Mutex
	texts []string
}

func (r *recordingNotifySender) Lookup(string) (PlatformReplier, bool) { return r, true }
func (r *recordingNotifySender) MaxReplyLength() int                   { return 4000 }
func (r *recordingNotifySender) Split(text string, _ int) []string     { return []string{text} }
func (r *recordingNotifySender) UsesSingleUseReplyToken() bool         { return false }
func (r *recordingNotifySender) Reply(_ context.Context, _, text string) (string, error) {
	r.mu.Lock()
	r.texts = append(r.texts, text)
	r.mu.Unlock()
	return "", nil
}

// noticesAfter waits for the async deliverNotice goroutines, then returns what
// reached the fake platform.
func (r *recordingNotifySender) noticesAfter(s *Scheduler) []string {
	s.triggerWG.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.texts...)
}

func newNoticeScheduler(t *testing.T, router SessionRouter) (*Scheduler, *recordingBroadcaster, *recordingNotifySender) {
	t.Helper()
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{
		MaxJobs:     5,
		ExecTimeout: 7 * time.Minute,
		StorePath:   t.TempDir() + "/cron_jobs.json",
	}, SchedulerDeps{Router: router, Telemetry: rec})
	ns := &recordingNotifySender{}
	s.configMapsPtr.Store(&cronConfigMaps{notifySender: ns})
	return s, rec, ns
}

func withNotify(a getSessionArgs, label string) getSessionArgs {
	a.notifyTo = NotifyTarget{Platform: "fake", ChatID: "chat-1"}
	a.snap.label = label
	a.runID = "9f8e7d6c5b4a3921"
	return a
}

// TestExecuteGetSession_CapacityIsSkipped: a GetOrCreate refusal wrapping
// ErrSessionCapacity is a skip with its own class — persisted onto the job so
// the dashboard shows it — and the notice says what to change.
func TestExecuteGetSession_CapacityIsSkipped(t *testing.T) {
	t.Parallel()
	capErr := fmt.Errorf("%w: %w", ErrSessionCapacity, errors.New("max exempt sessions reached: cron namespace (12)"))
	s, rec, ns := newNoticeScheduler(t, &fakeRouter{getErr: capErr})
	j := &Job{ID: "job-capacity", Schedule: "@every 5m"}
	s.putJobForTest(j)

	_, _, abort := s.executeGetSession(withNotify(newGetSessionArgs(t, s, j), "日报"))
	if !abort {
		t.Fatal("capacity refusal must abort the run")
	}
	if rec.endedCount() != 1 {
		t.Fatalf("want 1 ended event, got %d", rec.endedCount())
	}
	ev := rec.endedAtCron(0)
	if ev.State != RunStateSkipped || ev.ErrorClass != ErrClassSessionCapacity {
		t.Errorf("ended = (%q, %q), want (skipped, session_capacity)", ev.State, ev.ErrorClass)
	}
	if got := s.jobForTest(t, j.ID).LastErrorClass; got != ErrClassSessionCapacity {
		t.Errorf("persisted LastErrorClass = %q, want session_capacity", got)
	}
	want := "[Cron 日报] 定时任务会话数已达上限，本次已跳过；可错开执行时间或改为每次重置上下文 · run 9f8e7d6c"
	if got := ns.noticesAfter(s); len(got) != 1 || got[0] != want {
		t.Errorf("notices = %q, want [%q]", got, want)
	}
}

// TestExecuteGetSession_SessionErrorNotice: an ordinary spawn failure stays
// failed/session_error and names the spawn as the cause.
func TestExecuteGetSession_SessionErrorNotice(t *testing.T) {
	t.Parallel()
	s, rec, ns := newNoticeScheduler(t, &fakeRouter{getErr: errors.New("spawn boom")})
	j := &Job{ID: "job-spawn-fail", Schedule: "@every 5m"}
	s.putJobForTest(j)

	s.executeGetSession(withNotify(newGetSessionArgs(t, s, j), "日报"))
	ev := rec.endedAtCron(0)
	if ev.State != RunStateFailed || ev.ErrorClass != ErrClassSessionError {
		t.Errorf("ended = (%q, %q), want (failed, session_error)", ev.State, ev.ErrorClass)
	}
	want := "[Cron 日报] 启动会话失败 · run 9f8e7d6c"
	if got := ns.noticesAfter(s); len(got) != 1 || got[0] != want {
		t.Errorf("notices = %q, want [%q]", got, want)
	}
}

// TestExecSendError_NoticeNamesCause: the send-error terminal names the cause,
// and a deadline prints the job's budget.
func TestExecSendError_NoticeNamesCause(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"deadline", fmt.Errorf("send: %w", context.DeadlineExceeded), "[Cron 日报] 执行超时（超过 7m） · run 9f8e7d6c"},
		{"send error", errors.New("broken pipe"), "[Cron 日报] 执行失败（CLI 发送错误） · run 9f8e7d6c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, _, ns := newNoticeScheduler(t, &fakeRouter{})
			j := &Job{ID: "job-send-" + strings.ReplaceAll(tc.name, " ", "-"), Schedule: "@every 5m"}
			s.putJobForTest(j)
			ga := withNotify(newGetSessionArgs(t, s, j), "日报")
			s.execSendError(execSendArgs{runCtx: ga.runCtx, jobTimeout: s.execTimeout}, abortResult{}, tc.err, costledger.Increment{})
			if got := ns.noticesAfter(s); len(got) != 1 || got[0] != tc.want {
				t.Errorf("notices = %q, want [%q]", got, tc.want)
			}
		})
	}
}

// TestFreshPreflight_WorkDirNoticeCarriesRunID: the work-dir preflight skip
// notice now carries the run id like every other failure notice.
func TestFreshPreflight_WorkDirNoticeCarriesRunID(t *testing.T) {
	t.Parallel()
	s, _, ns := newNoticeScheduler(t, &fakeRouter{})
	j := &Job{ID: "job-workdir-gone", Schedule: "@every 5m", FreshContext: true}
	s.putJobForTest(j)
	ga := withNotify(newGetSessionArgs(t, s, j), "日报")
	ga.snap.fresh = true
	ga.snap.workDir = t.TempDir() + "/missing"

	if _, ok := s.freshContextPreflightP0(preflightArgs{runCtx: ga.runCtx}); ok {
		t.Fatal("unreachable work_dir must fail the preflight")
	}
	want := "[Cron 日报] 工作目录不可达，本次执行已跳过 · run 9f8e7d6c"
	if got := ns.noticesAfter(s); len(got) != 1 || got[0] != want {
		t.Errorf("notices = %q, want [%q]", got, want)
	}
}

// TestSandboxFailureNotice_NamesCause: sandbox terminals share the cause table,
// and a sandbox deadline prints the clamped sandbox budget.
func TestSandboxFailureNotice_NamesCause(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		execTimeout time.Duration
		state       RunState
		class       ErrorClass
		want        string
	}{
		{"failed", 7 * time.Minute, RunStateFailed, ErrClassSandboxFailed, "[Cron 日报] 云沙箱任务失败 · run 9f8e7d6c"},
		{"transport deadline", 7 * time.Minute, RunStateTimedOut, ErrClassSandboxTransport, "[Cron 日报] 云沙箱运行超时（超过 7m），任务状态未知，请检查执行历史 · run 9f8e7d6c"},
		{"deadline clamped", 2 * time.Hour, RunStateTimedOut, ErrClassSandboxTransport, "[Cron 日报] 云沙箱运行超时（超过 1h），任务状态未知，请检查执行历史 · run 9f8e7d6c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, _, ns := newNoticeScheduler(t, &fakeRouter{})
			s.execTimeout = tc.execTimeout
			j := &Job{ID: "job-sbx-" + strings.ReplaceAll(tc.name, " ", "-"), Schedule: "@every 5m"}
			s.putJobForTest(j)
			ga := withNotify(newGetSessionArgs(t, s, j), "日报")
			s.finishSandboxRun(sandboxExecArgs{runCtx: ga.runCtx}, tc.state, tc.class, "", "boom", nil)
			if got := ns.noticesAfter(s); len(got) != 1 || got[0] != tc.want {
				t.Errorf("notices = %q, want [%q]", got, tc.want)
			}
		})
	}
}
