package cron

import (
	"log/slog"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// registerStubByValue creates (or refreshes) a router session entry for the
// job so it appears in the dashboard workspace list. Safe to call without a
// router (tests). Callers must not be holding s.mu — RegisterCronStubWithChain
// re-enters router state.
//
// 当 lastSessionID 非空（最近一次成功执行的 session_id），会作为单元素
// chain 传给 stub，这样 dashboard 点击 cron 侧边栏时能按该 ID 从 claude
// 项目目录找到 JSONL 历史。否则 fresh_context=true 的定时任务每次 Reset
// 都会把 stub 的 chain 清空，事件面板就永远是空白。
//
// R232-CR-12 把原 registerStub(*Job) / registerStubByValue / stubChain 三
// 个仅参数差异的 helper 合成单个值参数版本：避免持锁路径误传 *Job 指针
// 后被并发 UpdateJob 改动；调用方一律先快照字段再传值。
//
// R241-ARCH-6 (#510): the historical "silent no-op when router is nil"
// hid wireup bugs because the misconfiguration only surfaced as a
// missing dashboard sidebar entry, not as a startup or first-tick
// failure. The construction-time slog.Error in NewScheduler now flags
// the missing wiring loud at boot; this callsite additionally logs the
// first time it would have refreshed a stub but couldn't (sync.Once
// gate so a router-less test fixture / AllowNilRouter deployment does
// not spam the log across N ticks).
// registerStubByValue creates / refreshes a router stub. Returns true on
// success, false when the scheduler has no router wired (the only silent
// "fail" path today). Per #491 (R247-GO-10) the bool flows back into
// EnsureStub so dashboard observability is honest about a no-op call;
// without this the sidebar reported "stub registered" while the router
// never saw the call.
func (s *Scheduler) registerStubByValue(id, workDir, prompt, lastSessionID string) bool {
	if s.router == nil {
		s.routerNilOnce.Do(func() {
			slog.Error("cron: registerStubByValue called without a router; dashboard sidebar will be empty for this scheduler — wireup bug or missing SchedulerDeps.Router?",
				"job_id", id)
		})
		return false
	}
	var chain []string
	if lastSessionID != "" {
		chain = []string{lastSessionID}
	}
	s.router.RegisterCronStubWithChain(sessionkey.CronKey(id), workDir, prompt, chain)
	return true
}

// registerStubFromJob 是 registerStubByValue 的便捷包装，对未持锁、且对
// *Job 字段稳定性已有把握（如 AddJob 后立刻调）的调用方简化字面。
// Return value mirrors registerStubByValue (#491).
func (s *Scheduler) registerStubFromJob(j *Job) bool {
	return s.registerStubByValue(j.ID, j.WorkDir, j.Prompt, j.LastSessionID)
}

// EnsureStub lazily (re-)registers a dashboard stub session for the given
// key (format "cron:<jobID>"). Returns true when the matching job still
// exists and a stub is now registered (either newly created or already
// present); returns false when the key is malformed, not a cron key, or
// the job is gone.
//
// Rationale: the sidebar "×" button routes through router.Remove and
// deletes the stub. Cron stubs are meant to be re-bornable — the next
// scheduled tick rebuilds them via executeJob's GetOrCreate — but between
// the dismissal and that tick, clicking the task card in the Cron panel
// would otherwise hit "session not found" because the WS subscribe path
// has nothing to attach to. This method is the idempotent recovery hook
// wired into handleSubscribe and /api/sessions/events.
// EnsureStub is safe to call on a nil *Scheduler: it returns false, matching
// the nil-safe pattern of NotifyDefault / StartedAt. This matters because a
// nil *Scheduler stored in a CronView interface is non-nil at the interface
// level, so a dashboard handler's `h.scheduler != nil` guard does not prevent
// calls on a typed-nil receiver. R20260603-ARCH-1.
func (s *Scheduler) EnsureStub(key string) bool {
	if s == nil {
		return false
	}
	if !sessionkey.IsCronKey(key) {
		return false
	}
	id := key[len(sessionkey.CronKeyPrefix):]
	if id == "" {
		return false
	}
	// Snapshot workDir/prompt under RLock, release before reaching into
	// router: RegisterCronStubWithChain calls notifyChange which fans out to
	// hub broadcasters, and holding s.mu across that path risks lock-order
	// inversion with the cron dispatcher (see ListAllJobsWithNextRun).
	s.mu.RLock()
	j, ok := s.jobs[id]
	var workDir, prompt, lastSessionID string
	if ok {
		workDir = j.WorkDir
		prompt = j.Prompt
		lastSessionID = j.LastSessionID
	}
	s.mu.RUnlock()
	if !ok {
		return false
	}
	// #491 (R247-GO-10): propagate the registerStubByValue success bool so
	// EnsureStub does not lie when the scheduler is router-less.
	return s.registerStubByValue(id, workDir, prompt, lastSessionID)
}

// resetRouterStub is the deferred router-side cleanup that pairs with
// deleteJobLocked. Caller MUST NOT hold s.mu — router.Reset re-enters
// router state and its notifyChange callback may take s.mu. Safe on a
// nil router (tests). R240-GO-1.
//
// R247-GO-11: also defensive against a nil receiver. Sibling getters
// (StartedAt / KnownSessionIDs) already short-circuit on a nil
// *Scheduler so test fixtures can construct a partial Scheduler and
// invoke deletion paths without dereferencing s. Without this guard a
// test calling DeleteJobByID on a zero-value scheduler — or production
// code that has not yet wired router — would NPE on s.router access
// rather than returning quietly.
func (s *Scheduler) resetRouterStub(jobID string) {
	if s == nil {
		return
	}
	if s.router == nil {
		return
	}
	s.router.Reset(sessionkey.CronKey(jobID))
}
