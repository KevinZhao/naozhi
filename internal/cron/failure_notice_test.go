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
		{"turn failed", ErrClassTurnFailed, RunStateFailed, 5 * time.Minute, "执行失败（后端报告本轮出错），请检查执行历史 · run 1a2b3c4d"},
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
			got := failureNoticeBody(jobSnapshot{}, tc.class, TurnCauseUnknown, tc.state, runID, tc.timeout)
			if got != tc.want {
				t.Errorf("failureNoticeBody(%q, %q) = %q, want %q", tc.class, tc.state, got, tc.want)
			}
			if strings.Contains(got, "请稍后重试") {
				t.Errorf("body %q still carries the generic retry advice", got)
			}
		})
	}
}

// allTurnCauses lists every named TurnCause; TestTurnFailedNotices_EveryCause
// holds it to the notice table.
var allTurnCauses = []TurnCause{
	TurnCauseMaxTurns, TurnCauseBudget, TurnCauseRefused, TurnCauseTruncated,
	TurnCauseContextTooLong, TurnCauseQuota, TurnCausePermission, TurnCauseBackendOverloaded,
	TurnCauseBackendRateLimited, TurnCauseBackendAuth, TurnCauseBackendInvalid,
	TurnCauseBackendUnreachable,
}

// TestFailureNoticeBody_TurnCause pins the turn_failed body per cause for a
// fresh-context job: each names what happened, none advises 继续 or /new
// (which in the notify chat would act on the wrong session), and an unnamed
// cause keeps the generic sentence.
func TestFailureNoticeBody_TurnCause(t *testing.T) {
	t.Parallel()
	const runID = "1a2b3c4d5e6f7a8b"
	cases := []struct {
		cause TurnCause
		want  string
	}{
		{TurnCauseUnknown, "执行失败（后端报告本轮出错），请检查执行历史 · run 1a2b3c4d"},
		{TurnCauseMaxTurns, "执行未完成（已达到最大执行步数），请检查执行历史 · run 1a2b3c4d"},
		{TurnCauseBudget, "执行未完成（已达到费用上限），请检查执行历史 · run 1a2b3c4d"},
		{TurnCauseRefused, "执行失败（模型拒绝了本次请求），请检查执行历史 · run 1a2b3c4d"},
		{TurnCauseTruncated, "执行未完成（回复超出模型单次输出上限），请检查执行历史 · run 1a2b3c4d"},
		{TurnCauseContextTooLong, "执行失败（对话上下文已超出模型上限），请检查执行历史 · run 1a2b3c4d"},
		{TurnCauseQuota, "执行失败（API 额度已用尽），请联系管理员 · run 1a2b3c4d"},
		{TurnCausePermission, "执行失败（请求被拒绝：权限或内容策略），请联系管理员 · run 1a2b3c4d"},
		{TurnCauseBackendOverloaded, "执行失败（后端服务负载较高），请检查执行历史 · run 1a2b3c4d"},
		{TurnCauseBackendRateLimited, "执行失败（后端调用过于频繁），请检查执行历史 · run 1a2b3c4d"},
		{TurnCauseBackendAuth, "执行失败（后端认证失败或凭证已过期），请联系管理员 · run 1a2b3c4d"},
		{TurnCauseBackendInvalid, "执行失败（后端无法处理本次请求），请检查执行历史 · run 1a2b3c4d"},
		{TurnCauseBackendUnreachable, "执行失败（连接模型服务超时或网络异常），请检查执行历史 · run 1a2b3c4d"},
		{TurnCause("from_a_newer_binary"), "执行失败（后端报告本轮出错），请检查执行历史 · run 1a2b3c4d"},
	}
	for _, tc := range cases {
		got := failureNoticeBody(jobSnapshot{fresh: true}, ErrClassTurnFailed, tc.cause, RunStateFailed, runID, 5*time.Minute)
		if got != tc.want {
			t.Errorf("cause %q: body = %q, want %q", tc.cause, got, tc.want)
		}
		for _, chatAdvice := range []string{"继续", "/new"} {
			if strings.Contains(got, chatAdvice) {
				t.Errorf("cause %q: body %q advises %q", tc.cause, got, chatAdvice)
			}
		}
	}
	// The cause only words a turn_failed run; any other class ignores it.
	if got := failureNoticeBody(jobSnapshot{}, ErrClassSendError, TurnCauseMaxTurns, RunStateFailed, runID, time.Minute); got != "执行失败（CLI 发送错误） · run 1a2b3c4d" {
		t.Errorf("send error with a cause: body = %q", got)
	}
}

// TestFailureNoticeBody_ContextTooLongByMode: a context-too-long failure of a
// job that keeps its context says to switch to reset-per-run, and where; a
// fresh job keeps the plain sentence, and other causes of a persistent job are
// unaffected.
func TestFailureNoticeBody_ContextTooLongByMode(t *testing.T) {
	t.Parallel()
	const (
		runID   = "1a2b3c4d5e6f7a8b"
		persist = "执行失败（对话上下文已超出模型上限）；该任务保留上下文，之后每次执行都会因此失败，可在控制台改为每次重置上下文"
	)
	cases := []struct {
		name  string
		snap  jobSnapshot
		cause TurnCause
		want  string
	}{
		{"fresh", jobSnapshot{fresh: true, platName: "feishu", chatID: "chat-1"}, TurnCauseContextTooLong,
			"执行失败（对话上下文已超出模型上限），请检查执行历史"},
		{"persistent dashboard", jobSnapshot{platName: "dashboard", chatID: "dash"}, TurnCauseContextTooLong, persist},
		{"persistent without source platform", jobSnapshot{}, TurnCauseContextTooLong, persist},
		{"persistent without source chat", jobSnapshot{platName: "feishu"}, TurnCauseContextTooLong, persist},
		{"persistent IM", jobSnapshot{platName: "feishu", chatID: "chat-1"}, TurnCauseContextTooLong,
			persist + "，或删除后不带 --keep-context 重新创建"},
		{"persistent IM, other cause", jobSnapshot{platName: "feishu", chatID: "chat-1"}, TurnCauseMaxTurns,
			"执行未完成（已达到最大执行步数），请检查执行历史"},
	}
	for _, tc := range cases {
		got := failureNoticeBody(tc.snap, ErrClassTurnFailed, tc.cause, RunStateFailed, runID, 5*time.Minute)
		if want := tc.want + " · run 1a2b3c4d"; got != want {
			t.Errorf("%s: body = %q, want %q", tc.name, got, want)
		}
		for _, chatAdvice := range []string{"继续", "/new"} {
			if strings.Contains(got, chatAdvice) {
				t.Errorf("%s: body %q advises %q", tc.name, got, chatAdvice)
			}
		}
	}
}

// TestExecuteOpt_ContextTooLongNoticeByMode: the run's snapshot reaches the
// notice, so a real context-too-long run words it by the job's mode and
// platform, and the auto-pause sentence still follows the run id.
func TestExecuteOpt_ContextTooLongNoticeByMode(t *testing.T) {
	t.Parallel()
	const (
		plain   = "执行失败（对话上下文已超出模型上限），请检查执行历史"
		persist = "执行失败（对话上下文已超出模型上限）；该任务保留上下文，之后每次执行都会因此失败，可在控制台改为每次重置上下文"
	)
	cases := []struct {
		name      string
		platform  string
		fresh     bool
		threshold int
		want      string
		suffix    func(id string) string
	}{
		{"fresh IM", "feishu", true, 0, plain, nil},
		{"persistent IM", "feishu", false, 0, persist + "，或删除后不带 --keep-context 重新创建", nil},
		{"persistent dashboard", "dashboard", false, 0, persist, nil},
		{"persistent IM, auto-paused", "feishu", false, 1, persist + "，或删除后不带 --keep-context 重新创建",
			func(id string) string {
				return "；已连续失败 1 次，任务已自动暂停，修复后发送 /cron resume " + id + " 恢复"
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, r, ns, id := newAutoPauseScheduler(t, tc.threshold, tc.platform)
			s.editJobForTest(t, id, func(j *Job) { j.FreshContext = tc.fresh })
			r.set(nil, &TurnFailedError{Cause: TurnCauseContextTooLong})
			runN(s, id, 1)
			got := ns.noticesAfter(s)
			if len(got) != 1 || !strings.HasPrefix(got[0], "[Cron ping] "+tc.want+" · run ") {
				t.Fatalf("notices = %q, want one starting %q", got, "[Cron ping] "+tc.want+" · run ")
			}
			if tc.suffix != nil && !strings.HasSuffix(got[0], tc.suffix(id)) {
				t.Errorf("notice = %q, want it to end %q", got[0], tc.suffix(id))
			}
		})
	}
}

// TestTurnFailedNotices_EveryCause: every named cause has its own sentence,
// distinct from the generic one and from every other cause's.
func TestTurnFailedNotices_EveryCause(t *testing.T) {
	t.Parallel()
	generic := turnFailedNotice(TurnCauseUnknown)
	seen := map[string]TurnCause{}
	for _, c := range allTurnCauses {
		s := turnFailedNotice(c)
		if s == generic {
			t.Errorf("cause %q falls back to the generic sentence", c)
		}
		if prev, dup := seen[s]; dup {
			t.Errorf("causes %q and %q share the sentence %q", prev, c, s)
		}
		seen[s] = c
	}
	if len(turnFailedNotices) != len(allTurnCauses) {
		t.Errorf("turnFailedNotices has %d entries, allTurnCauses %d: keep the two in step", len(turnFailedNotices), len(allTurnCauses))
	}
}

func TestFailureNoticeBody_ShortAndMissingRunID(t *testing.T) {
	t.Parallel()
	if got := failureNoticeBody(jobSnapshot{}, ErrClassSessionError, TurnCauseUnknown, RunStateFailed, "abc", time.Minute); got != "启动会话失败 · run abc" {
		t.Errorf("short run id: got %q", got)
	}
	if got := failureNoticeBody(jobSnapshot{}, ErrClassSessionError, TurnCauseUnknown, RunStateFailed, "", time.Minute); got != "启动会话失败" {
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
			s.execSendError(execSendArgs{runCtx: ga.runCtx, jobTimeout: s.execTimeout}, abortResult{}, tc.err, costledger.Increment{}, "")
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
